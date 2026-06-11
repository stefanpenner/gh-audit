package github

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// enrichCommitFanout bounds concurrent per-commit enrichments inside a single
// EnrichCommits batch. The outer pipeline already bounds batch concurrency,
// so this cap keeps goroutine growth linear in batch size without flooding.
const enrichCommitFanout = 10

// enrichPRFanout bounds concurrent per-PR work within a single commit's
// enrichment (PR detail, reviews, check runs, PR commits).
const enrichPRFanout = 5

// APIStats tracks API request counts by endpoint.
type APIStats struct {
	// CommitDetailEager is GET /commits/{sha} fetched during enrichment
	// (the getCommit defensive fallback when DB doesn't have the row
	// pre-populated). On warm DBs this should be near zero.
	CommitDetailEager atomic.Int64
	// CommitDetailLazyEmpty is GET /commits/{sha} fired by audit rule
	// §2's empty-commit fallback when an otherwise non-compliant
	// commit looks zero-stat locally and we need to confirm before
	// firing the empty-commit waiver. Dominant when many commits
	// land without additions/deletions populated (cold sweep).
	CommitDetailLazyEmpty atomic.Int64
	// CommitDetailLazySelf is GET /commits/{sha} fired by audit rule
	// §5's PR-branch-author empty-stats disambiguation when a
	// reviewer's PR-branch commits all look zero-stat locally and
	// we need to know whether they actually contributed code. Lower
	// volume than the §2 path; only fires when reviewer-as-author
	// appears.
	CommitDetailLazySelf atomic.Int64
	// CommitDetailLazyExempt is GET /commits/{sha} fired by audit rule
	// §1's PR-branch emptiness verification: a non-exempt branch commit
	// in an exempt author's squash looks zero-stat locally and the
	// carve-out needs proof it truly shipped no code.
	CommitDetailLazyExempt atomic.Int64
	CommitPRs              atomic.Int64
	PRDetail               atomic.Int64
	Reviews                atomic.Int64
	CheckRuns              atomic.Int64
	PRCommits              atomic.Int64
	RevertVerification     atomic.Int64 // GetCommitFiles calls made for clean-revert diff check
	CacheHits              atomic.Int64
	DBHits                 atomic.Int64
	// PRRecovered counts commit→PR links discovered through the
	// parse-then-canonical-verify fallback when GitHub's commit→PR
	// reverse index returned empty. Each increment represents one
	// would-be "no associated pull request" that was correctly
	// linked back to its merged PR via PullRequest.merge_commit_sha.
	// Not added to Total() — recovery rides on top of the existing
	// PRDetail call, no extra endpoint cost.
	PRRecovered atomic.Int64

	// Duration counters per endpoint, expressed in nanoseconds.
	// Total/count gives mean wall-time per call — coarse but
	// enough to spot a slow endpoint dragging the sweep tail. We
	// don't yet track p50/p99 (would need histograms); mean is
	// the cheap-to-add first cut. Each counter accumulates across
	// the whole pipeline run, so derive averages by dividing by
	// the matching count counter at the same point in time.
	CommitDetailEagerNanos      atomic.Int64
	CommitDetailLazyEmptyNanos  atomic.Int64
	CommitDetailLazySelfNanos   atomic.Int64
	CommitDetailLazyExemptNanos atomic.Int64
	CommitPRsNanos              atomic.Int64
	PRDetailNanos               atomic.Int64
	ReviewsNanos                atomic.Int64
	CheckRunsNanos              atomic.Int64
	PRCommitsNanos              atomic.Int64
	RevertVerificationNanos     atomic.Int64
}

// Total returns the total number of API requests made.
func (s *APIStats) Total() int64 {
	return s.CommitDetailEager.Load() +
		s.CommitDetailLazyEmpty.Load() + s.CommitDetailLazySelf.Load() +
		s.CommitDetailLazyExempt.Load() +
		s.CommitPRs.Load() + s.PRDetail.Load() +
		s.Reviews.Load() + s.CheckRuns.Load() + s.PRCommits.Load() +
		s.RevertVerification.Load()
}

// EnrichmentCache provides read access to previously-synced enrichment data.
// Implemented by *db.DB. Merged PR data is immutable, so DB results are
// always valid and eliminate the need for HTTP-level ETag caching.
type EnrichmentCache interface {
	// GetPullRequest returns the DB-stored row for org/repo#number or
	// (nil, nil) if absent. Drives the merged-PR freeze: a row with
	// Merged=true is immutable in every field gh-audit cares about, so
	// the API call is skipped entirely on subsequent runs.
	GetPullRequest(ctx context.Context, org, repo string, number int) (*model.PullRequest, error)
	GetPRsForCommit(ctx context.Context, org, repo, sha string) ([]model.PullRequest, error)
	GetReviewsForPR(ctx context.Context, org, repo string, prNumber int) ([]model.Review, error)
	GetCheckRunsForCommit(ctx context.Context, org, repo, sha string) ([]model.CheckRun, error)
	GetCommitsForPR(ctx context.Context, org, repo string, prNumber int) ([]model.Commit, error)
	// GetCommitsBySHA returns the DB-stored commit rows for the given SHAs.
	// Used by enrichment to avoid a redundant GetCommitDetail call — the
	// list-commits response already populated message/author/parent_count,
	// and additions/deletions are resolved lazily via the audit's
	// empty-commit fallback when needed.
	GetCommitsBySHA(ctx context.Context, org, repo string, shas []string) ([]model.Commit, error)
}

// A CachingEnricher wraps a Client with per-run in-memory caches and an
// optional DB fallback for cross-run caching. Two commits referencing the
// same PR reuse the cached result instead of hitting the API twice.
//
// The reverse-lookup index maps merge_commit_sha → PR numbers so that
// ListCommitPullRequests can be skipped when the commit's SHA matches a
// known PR's merge commit.
//
//	Client ──┐
//	DB cache ──→ CachingEnricher ──→ EnrichmentResult
//	             ├── in-memory cache (PRs, reviews, checks, commits)
//	             └── reverse PR index (merge_commit_sha → PR numbers)
type CachingEnricher struct {
	client  *Client
	dbCache EnrichmentCache
	Stats   APIStats

	// requiredCheckNames (lowercased) drives the legacy-status supplement:
	// when a freshly-fetched check-run list is missing one of these names,
	// the combined commit-status API is consulted and its contexts merged
	// in as synthetic CheckRuns — CI that reports via /statuses (older
	// Jenkins) is otherwise invisible to §6 and would read permanently
	// "missing". Empty set disables the extra call entirely.
	requiredCheckNames map[string]struct{}

	mu             sync.Mutex
	prCache        map[string]*model.PullRequest
	reviewCache    map[string][]model.Review
	checkRunCache  map[string][]model.CheckRun
	prCommitCache  map[string][]model.Commit
	commitPRCache  map[string][]model.PullRequest // sha → PRs (from API or reverse index)
	mergeCommitIdx map[string][]int               // merge_commit_sha → PR numbers
}

// NewCachingEnricher creates a new caching enricher. If dbCache is non-nil,
// it is consulted for previously-synced data before making API calls.
func NewCachingEnricher(client *Client, dbCache EnrichmentCache) *CachingEnricher {
	return &CachingEnricher{
		client:             client,
		dbCache:            dbCache,
		prCache:            make(map[string]*model.PullRequest),
		reviewCache:        make(map[string][]model.Review),
		checkRunCache:      make(map[string][]model.CheckRun),
		prCommitCache:      make(map[string][]model.Commit),
		commitPRCache:      make(map[string][]model.PullRequest),
		mergeCommitIdx:     make(map[string][]int),
		requiredCheckNames: make(map[string]struct{}),
	}
}

// indexPR adds a PR's merge_commit_sha to the reverse-lookup index.
func (ce *CachingEnricher) indexPR(pr *model.PullRequest) {
	if pr.MergeCommitSHA == "" {
		return
	}
	key := fmt.Sprintf("%s/%s/%s", pr.Org, pr.Repo, pr.MergeCommitSHA)
	// Check if already indexed
	for _, n := range ce.mergeCommitIdx[key] {
		if n == pr.Number {
			return
		}
	}
	ce.mergeCommitIdx[key] = append(ce.mergeCommitIdx[key], pr.Number)
}

// getCommit resolves the commit's metadata (message, author, parent count,
// etc.) without calling GitHub. The DB row is written by UpsertCommits
// during the list-commits phase, which runs before EnrichCommits, so on a
// normal pipeline invocation the row is always present. The fallback to
// GetCommitDetail is kept for defensive reasons (tests that invoke the
// enricher without a pre-populated store); it counts as a CommitDetail
// stat so the telemetry still surfaces any unexpected hits.
func (ce *CachingEnricher) getCommit(ctx context.Context, org, repo, sha string) (*model.Commit, error) {
	if ce.dbCache != nil {
		if commits, err := ce.dbCache.GetCommitsBySHA(ctx, org, repo, []string{sha}); err == nil && len(commits) > 0 {
			c := commits[0]
			ce.Stats.DBHits.Add(1)
			return &c, nil
		}
	}
	ce.Stats.CommitDetailEager.Add(1)
	start := time.Now()
	defer func() { ce.Stats.CommitDetailEagerNanos.Add(time.Since(start).Nanoseconds()) }()
	return ce.client.GetCommitDetail(ctx, org, repo, sha)
}

func (ce *CachingEnricher) getPRsForCommit(ctx context.Context, org, repo, sha string) ([]model.PullRequest, error) {
	cacheKey := fmt.Sprintf("%s/%s/%s", org, repo, sha)

	ce.mu.Lock()
	if cached, ok := ce.commitPRCache[cacheKey]; ok {
		ce.mu.Unlock()
		ce.Stats.CacheHits.Add(1)
		return cached, nil
	}

	// Reverse-lookup: check if this SHA is a known merge_commit_sha
	if prNumbers, ok := ce.mergeCommitIdx[cacheKey]; ok {
		var prs []model.PullRequest
		for _, num := range prNumbers {
			prKey := fmt.Sprintf("%s/%s/%d", org, repo, num)
			if pr, ok := ce.prCache[prKey]; ok {
				prs = append(prs, *pr)
			}
		}
		if len(prs) > 0 {
			ce.commitPRCache[cacheKey] = prs
			ce.mu.Unlock()
			ce.Stats.CacheHits.Add(1)
			return prs, nil
		}
	}
	ce.mu.Unlock()

	// DB fallback: check if we have PRs for this commit from a previous run
	if ce.dbCache != nil {
		prs, err := ce.dbCache.GetPRsForCommit(ctx, org, repo, sha)
		if err == nil && len(prs) > 0 {
			ce.mu.Lock()
			ce.commitPRCache[cacheKey] = prs
			ce.mu.Unlock()
			ce.Stats.DBHits.Add(1)
			return prs, nil
		}
	}

	ce.Stats.CommitPRs.Add(1)
	startCommitPRs := time.Now()
	prs, err := ce.client.ListCommitPullRequests(ctx, org, repo, sha)
	ce.Stats.CommitPRsNanos.Add(time.Since(startCommitPRs).Nanoseconds())
	if err != nil {
		return nil, err
	}

	// Recovery fallback: GitHub's /commits/{sha}/pulls endpoint is a
	// best-effort reverse index, not a canonical relationship. It
	// sporadically returns empty for commits whose merged PR clearly
	// exists (verified empirically via PullRequest.merge_commit_sha).
	// When the API hands back zero PRs, parse the trailing `(#N)` from
	// the squash-merge commit message and *verify canonically*: fetch
	// PR #N and accept the link only if its merge_commit_sha matches
	// this SHA. The parse step is forgeable; the verify step is not —
	// only GitHub sets merge_commit_sha on a real merge event.
	if len(prs) == 0 {
		if recovered, ok := ce.recoverPRFromMergeMessage(ctx, org, repo, sha); ok {
			prs = []model.PullRequest{*recovered}
			ce.Stats.PRRecovered.Add(1)
		}
	}

	ce.mu.Lock()
	ce.commitPRCache[cacheKey] = prs
	ce.mu.Unlock()
	return prs, nil
}

// recoverPRFromMergeMessage attempts to repair a missing commit→PR link
// when GitHub's reverse index returned empty. Implements rule §3's
// canonical-verify mitigation:
//
//  1. Resolve the commit message (DB-cached via getCommit).
//  2. Parse the trailing `(#N)` token from the first line.
//  3. Fetch PR #N via getPR (merged-PR-frozen via DB cache).
//  4. Accept the link iff pr.Merged && pr.MergeCommitSHA == sha.
//
// Any failure or mismatch returns (nil, false) so rule §3 fires
// unchanged. We never silently accept an unverified link — the parse
// step is a hint, the verify step is the trust boundary.
func (ce *CachingEnricher) recoverPRFromMergeMessage(ctx context.Context, org, repo, sha string) (*model.PullRequest, bool) {
	commit, err := ce.getCommit(ctx, org, repo, sha)
	if err != nil || commit == nil {
		return nil, false
	}
	number, ok := ParsePRReference(commit.Message)
	if !ok {
		return nil, false
	}
	pr, err := ce.getPR(ctx, org, repo, number)
	if err != nil || pr == nil {
		return nil, false
	}
	if !pr.Merged || pr.MergeCommitSHA != sha {
		return nil, false
	}
	return pr, true
}

func (ce *CachingEnricher) getPR(ctx context.Context, org, repo string, number int) (*model.PullRequest, error) {
	key := fmt.Sprintf("%s/%s/%d", org, repo, number)

	ce.mu.Lock()
	if cached, ok := ce.prCache[key]; ok {
		ce.mu.Unlock()
		ce.Stats.CacheHits.Add(1)
		return cached, nil
	}
	ce.mu.Unlock()

	// DB fallback: a previously-synced merged PR is frozen — every field
	// gh-audit consumes (head_sha, author, merged_by, merged_at) is
	// immutable post-merge, so the API call is wasted budget. Open or
	// not-yet-synced PRs fall through to the live fetch.
	if ce.dbCache != nil {
		pr, err := ce.dbCache.GetPullRequest(ctx, org, repo, number)
		if err == nil && pr != nil && pr.Merged {
			ce.mu.Lock()
			ce.prCache[key] = pr
			ce.indexPR(pr)
			ce.mu.Unlock()
			ce.Stats.DBHits.Add(1)
			return pr, nil
		}
	}

	ce.Stats.PRDetail.Add(1)
	startPRDetail := time.Now()
	pr, err := ce.client.GetPullRequest(ctx, org, repo, number)
	ce.Stats.PRDetailNanos.Add(time.Since(startPRDetail).Nanoseconds())
	if err != nil {
		return nil, err
	}

	ce.mu.Lock()
	ce.prCache[key] = pr
	ce.indexPR(pr)
	ce.mu.Unlock()
	return pr, nil
}

// SetRequiredCheckNames declares the configured §6 required-check names.
// Used to decide when the legacy commit-status supplement is worth an
// extra API call; matching is case-insensitive, mirroring
// evaluateRequiredChecks.
func (ce *CachingEnricher) SetRequiredCheckNames(names []string) {
	ce.requiredCheckNames = make(map[string]struct{}, len(names))
	for _, n := range names {
		ce.requiredCheckNames[strings.ToLower(n)] = struct{}{}
	}
}

// missingRequiredCheck reports whether any configured required check name
// is absent from runs.
func (ce *CachingEnricher) missingRequiredCheck(runs []model.CheckRun) bool {
	if len(ce.requiredCheckNames) == 0 {
		return false
	}
	present := make(map[string]struct{}, len(runs))
	for _, r := range runs {
		present[strings.ToLower(r.CheckName)] = struct{}{}
	}
	for name := range ce.requiredCheckNames {
		if _, ok := present[name]; !ok {
			return true
		}
	}
	return false
}

// isPRMerged reports whether the PR was *previously synced* and is in
// a merged state — the only situation where a zero-row reviews / check
// runs / PR-branch-commits result from the DB is authoritative.
//
// Critically, this MUST NOT consult the in-memory prCache. That cache
// is also populated by a fresh API fetch in getPR; trusting it here
// would let the freeze fire on a PR whose sub-data (reviews, etc.) has
// not yet been persisted, silently dropping every API call we needed.
// On a never-before-seen month that surfaced as ~55% of merged PRs
// being marked "no approval on final commit" because the reviews fetch
// was skipped.
//
// Going straight to the DB makes "row exists with merged=true" the
// proxy for "previously synced and finalised", which is what the
// freeze actually depends on. DuckDB PK lookups are sub-millisecond.
// A DB error or missing row falls through to the API path.
func (ce *CachingEnricher) isPRMerged(ctx context.Context, org, repo string, prNumber int) bool {
	if ce.dbCache == nil {
		return false
	}
	pr, err := ce.dbCache.GetPullRequest(ctx, org, repo, prNumber)
	if err != nil || pr == nil {
		return false
	}
	return pr.Merged
}

// getReviews fetches the reviews for a PR, preferring the in-memory cache,
// then the DB (under the merged-PR freeze), then the API.
//
// Known limitation: reviews CAN change after merge — a post-merge dismissal
// (exactly what HasPostMergeConcern detects) lands on an already-merged PR.
// Because the merged-PR freeze treats the first synced snapshot as final,
// review changes that happen after the first sync are not observed; picking
// them up requires re-syncing the window. The freeze is kept regardless:
// removing it re-opens the ~55% false-flag mode documented on isPRMerged
// and would re-fetch reviews for every merged PR on every run (an API
// storm). Revisit deliberately, not here.
func (ce *CachingEnricher) getReviews(ctx context.Context, org, repo string, prNumber int) ([]model.Review, error) {
	key := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)

	ce.mu.Lock()
	if cached, ok := ce.reviewCache[key]; ok {
		ce.mu.Unlock()
		ce.Stats.CacheHits.Add(1)
		return cached, nil
	}
	ce.mu.Unlock()

	// DB fallback — but ONLY under the merged-PR freeze, regardless of
	// row count. For a merged PR the stored snapshot (including a
	// zero-row result) is authoritative. Rows for a non-merged PR are a
	// moment-in-time copy: trusting them would hide reviews submitted
	// after the snapshot (false "no approval on final commit"). No
	// current writer persists open-PR reviews, but the freeze must not
	// depend on that invariant holding forever.
	if ce.dbCache != nil {
		reviews, err := ce.dbCache.GetReviewsForPR(ctx, org, repo, prNumber)
		if err == nil && ce.isPRMerged(ctx, org, repo, prNumber) {
			ce.mu.Lock()
			ce.reviewCache[key] = reviews
			ce.mu.Unlock()
			ce.Stats.DBHits.Add(1)
			return reviews, nil
		}
	}

	ce.Stats.Reviews.Add(1)
	startReviews := time.Now()
	reviews, err := ce.client.ListReviews(ctx, org, repo, prNumber)
	ce.Stats.ReviewsNanos.Add(time.Since(startReviews).Nanoseconds())
	if err != nil {
		return nil, err
	}

	ce.mu.Lock()
	ce.reviewCache[key] = reviews
	ce.mu.Unlock()
	return reviews, nil
}

// getCheckRuns fetches the check runs for a head SHA. The prNumber
// argument is only used for the merged-PR freeze decision: when the PR
// is merged, head SHA is final and any check runs that were going to run
// have already run (the GH check API does not retroactively add new runs
// to a merged PR's head SHA), so a DB-empty result is the truth.
func (ce *CachingEnricher) getCheckRuns(ctx context.Context, org, repo, ref string, prNumber int) ([]model.CheckRun, error) {
	key := fmt.Sprintf("%s/%s/%s", org, repo, ref)

	ce.mu.Lock()
	if cached, ok := ce.checkRunCache[key]; ok {
		ce.mu.Unlock()
		ce.Stats.CacheHits.Add(1)
		return cached, nil
	}
	ce.mu.Unlock()

	// DB fallback — only under the merged-PR freeze AND only when every
	// persisted run has reached its terminal "completed" status. Runs
	// stored as queued/in_progress were snapshotted mid-flight; an open
	// PR's head can also gain re-runs. Either condition failing forces a
	// refetch; the sync pipeline persists the refreshed rows on its
	// normal write path.
	if ce.dbCache != nil {
		runs, err := ce.dbCache.GetCheckRunsForCommit(ctx, org, repo, ref)
		if err == nil && ce.isPRMerged(ctx, org, repo, prNumber) && allCheckRunsCompleted(runs) {
			ce.mu.Lock()
			ce.checkRunCache[key] = runs
			ce.mu.Unlock()
			ce.Stats.DBHits.Add(1)
			return runs, nil
		}
	}

	ce.Stats.CheckRuns.Add(1)
	startCheckRuns := time.Now()
	runs, err := ce.client.ListCheckRunsForRef(ctx, org, repo, ref)
	ce.Stats.CheckRunsNanos.Add(time.Since(startCheckRuns).Nanoseconds())
	if err != nil {
		return nil, err
	}

	// Legacy-status supplement: only when a configured required check is
	// absent from the Checks-API results — the common all-Checks-API case
	// costs nothing extra. Counted under the CheckRuns stats (it is a
	// check-source call). Synthetic rows flow into the enrichment result
	// and persist to check_runs like any other run, so the merged-PR
	// freeze covers them on later reads.
	if ce.missingRequiredCheck(runs) {
		ce.Stats.CheckRuns.Add(1)
		startStatuses := time.Now()
		statuses, serr := ce.client.ListStatusContexts(ctx, org, repo, ref)
		ce.Stats.CheckRunsNanos.Add(time.Since(startStatuses).Nanoseconds())
		if serr != nil {
			return nil, serr
		}
		runs = append(runs, statuses...)
	}

	ce.mu.Lock()
	ce.checkRunCache[key] = runs
	ce.mu.Unlock()
	return runs, nil
}

// allCheckRunsCompleted reports whether every run has reached the terminal
// "completed" status. An empty slice is trivially complete (the merged-PR
// freeze's authoritative-empty case).
func allCheckRunsCompleted(runs []model.CheckRun) bool {
	for _, r := range runs {
		if r.Status != "completed" {
			return false
		}
	}
	return true
}

func (ce *CachingEnricher) getPRCommits(ctx context.Context, org, repo string, prNumber int) ([]model.Commit, error) {
	key := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)

	ce.mu.Lock()
	if cached, ok := ce.prCommitCache[key]; ok {
		ce.mu.Unlock()
		ce.Stats.CacheHits.Add(1)
		return cached, nil
	}
	ce.mu.Unlock()

	// DB fallback — only under the merged-PR freeze, regardless of row
	// count: branch commits of a non-merged PR can still grow (a later
	// push), and trusting a snapshot would blind §1/§5 contributor
	// analysis to them. The zero-row arm only matters for empty PRs
	// that somehow got merged.
	if ce.dbCache != nil {
		commits, err := ce.dbCache.GetCommitsForPR(ctx, org, repo, prNumber)
		if err == nil && ce.isPRMerged(ctx, org, repo, prNumber) {
			ce.mu.Lock()
			ce.prCommitCache[key] = commits
			ce.mu.Unlock()
			ce.Stats.DBHits.Add(1)
			return commits, nil
		}
	}

	ce.Stats.PRCommits.Add(1)
	startPRCommits := time.Now()
	commits, err := ce.client.ListPRCommits(ctx, org, repo, prNumber)
	ce.Stats.PRCommitsNanos.Add(time.Since(startPRCommits).Nanoseconds())
	if err != nil {
		return nil, err
	}

	ce.mu.Lock()
	ce.prCommitCache[key] = commits
	ce.mu.Unlock()
	return commits, nil
}

// EnrichCommits fetches enrichment data for a batch of commits, using
// in-memory cache, reverse PR index, and DB fallback before hitting the API.
//
// Commits in the batch are enriched concurrently (bounded by enrichCommitFanout)
// and PRs within a single commit are also fetched concurrently (bounded by
// enrichPRFanout). The caching layer's mutex serializes cache map access, so
// concurrent enrichment is safe.
func (ce *CachingEnricher) EnrichCommits(ctx context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	results := make([]model.EnrichmentResult, len(shas))

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(enrichCommitFanout)

	for i, sha := range shas {
		eg.Go(func() error {
			res, err := ce.enrichOneCommit(ectx, org, repo, sha)
			if err != nil {
				return fmt.Errorf("commit %s: %w", sha[:12], err)
			}
			results[i] = res
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// prEnrichment holds the per-PR fetch results so the assembly loop can combine
// them deterministically (check-run dedup preserves ordering).
type prEnrichment struct {
	fullPR        *model.PullRequest
	reviews       []model.Review
	checkRuns     []model.CheckRun
	branchCommits []model.Commit
}

// enrichOneCommit fans out across a commit's PRs; each PR's detail, reviews,
// check runs, and branch commits are fetched in the same goroutine so we
// don't explode goroutine count by 4× for tiny wins.
func (ce *CachingEnricher) enrichOneCommit(ctx context.Context, org, repo, sha string) (model.EnrichmentResult, error) {
	// Recover the commit's metadata (message/author/parent_count) from the
	// DB — the list-commits phase already persisted everything we need
	// except additions/deletions, and those are resolved lazily by the
	// audit's empty-commit fallback. Historically we called
	// GetCommitDetail eagerly here; on a full-org sweep that accounted for
	// ~16% of all REST calls and was wasted on every commit that passed
	// the PR-approval path (most of them).
	detail, err := ce.getCommit(ctx, org, repo, sha)
	if err != nil {
		return model.EnrichmentResult{}, err
	}

	prs, err := ce.getPRsForCommit(ctx, org, repo, sha)
	if err != nil {
		return model.EnrichmentResult{}, fmt.Errorf("PRs: %w", err)
	}

	enrichments := make([]prEnrichment, len(prs))

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(enrichPRFanout)

	for j := range prs {
		pr := prs[j]
		eg.Go(func() error {
			fullPR, err := ce.getPR(ectx, org, repo, pr.Number)
			if err != nil {
				return fmt.Errorf("PR #%d detail: %w", pr.Number, err)
			}
			enrichments[j].fullPR = fullPR

			reviews, err := ce.getReviews(ectx, org, repo, pr.Number)
			if err != nil {
				return fmt.Errorf("PR #%d reviews: %w", pr.Number, err)
			}
			enrichments[j].reviews = reviews

			headSHA := pr.HeadSHA
			if fullPR.HeadSHA != "" {
				headSHA = fullPR.HeadSHA
			}
			if headSHA != "" {
				runs, err := ce.getCheckRuns(ectx, org, repo, headSHA, pr.Number)
				if err != nil {
					return fmt.Errorf("PR #%d check runs: %w", pr.Number, err)
				}
				enrichments[j].checkRuns = runs
			}

			branchCommits, err := ce.getPRCommits(ectx, org, repo, pr.Number)
			if err != nil {
				return fmt.Errorf("PR #%d pr-commits: %w", pr.Number, err)
			}
			enrichments[j].branchCommits = branchCommits
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return model.EnrichmentResult{}, err
	}

	var allReviews []model.Review
	var allCheckRuns []model.CheckRun
	prBranchCommits := make(map[int][]model.Commit)
	seenCheckRef := make(map[string]bool)

	for j := range prs {
		e := enrichments[j]
		if e.fullPR != nil {
			prs[j].MergedByLogin = e.fullPR.MergedByLogin
			prs[j].MergedByID = e.fullPR.MergedByID
			if e.fullPR.HeadSHA != "" {
				prs[j].HeadSHA = e.fullPR.HeadSHA
			}
			prs[j].HeadBranch = e.fullPR.HeadBranch
		}
		allReviews = append(allReviews, e.reviews...)
		if prs[j].HeadSHA != "" && !seenCheckRef[prs[j].HeadSHA] {
			seenCheckRef[prs[j].HeadSHA] = true
			allCheckRuns = append(allCheckRuns, e.checkRuns...)
		}
		prBranchCommits[prs[j].Number] = e.branchCommits
	}

	result := model.EnrichmentResult{
		Commit:          *detail,
		PRs:             prs,
		Reviews:         allReviews,
		CheckRuns:       allCheckRuns,
		PRBranchCommits: prBranchCommits,
	}

	// Classify & verify clean-revert and clean-merge status. Both may need
	// this commit's file patches (revert verification for manual reverts,
	// merge verification to detect conflict-resolution edits). We fetch
	// those files at most once per commit and share between classifiers.
	ce.classifyRevertAndMerge(ctx, org, repo, sha, detail.ParentCount, &result)

	return result, nil
}

// classifyRevertAndMerge populates the revert-classification and
// merge-classification fields on the enrichment. The two checks share the
// same GetCommitFiles(current commit) call when both need it, so the total
// cost is at most one extra API call for merge classification plus one more
// for a diff-verified manual revert.
func (ce *CachingEnricher) classifyRevertAndMerge(
	ctx context.Context,
	org, repo, sha string,
	parentCount int,
	result *model.EnrichmentResult,
) {
	// Lazily fetch this commit's own files; reused by both classifiers.
	var ownFiles []model.FileDiff
	var ownFilesErr error
	var ownFetched bool
	fetchOwn := func() ([]model.FileDiff, error) {
		if ownFetched {
			return ownFiles, ownFilesErr
		}
		ownFetched = true
		ce.Stats.RevertVerification.Add(1)
		startRevertVerify := time.Now()
		ownFiles, ownFilesErr = ce.client.GetCommitFiles(ctx, org, repo, sha)
		ce.Stats.RevertVerificationNanos.Add(time.Since(startRevertVerify).Nanoseconds())
		return ownFiles, ownFilesErr
	}

	// --- Revert classification ---
	kind, _ := ParseRevert(result.Commit.Message)
	switch kind {
	case NotRevert, RevertOfRevert:
		result.RevertVerification = "none"
	case AutoRevert:
		// AutoRevert's two SHAs come straight from the message; no
		// branch-name fallback needed.
		_, sha := ParseRevert(result.Commit.Message)
		result.IsCleanRevert = true
		result.RevertVerification = "message-only"
		result.RevertedSHA = sha
	case ManualRevert:
		// Prefer the `This reverts commit <sha>` trailer (what `git revert`
		// emits). Fall back to GitHub's `revert-<N>-<base-branch>` head-
		// branch convention for commits produced by the "Revert" button,
		// which doesn't emit the trailer.
		revertedSHA, err := ResolveRevertedSHA(result.Commit.Message, result.PRs, func(n int) (*model.PullRequest, error) {
			return ce.getPR(ctx, org, repo, n)
		})
		if err != nil {
			// Transient lookup failure — treat as unverifiable rather
			// than letting the error bubble out and fail enrichment.
			result.RevertVerification = "message-only"
			break
		}
		result.RevertedSHA = revertedSHA
		if revertedSHA == "" {
			result.RevertVerification = "message-only"
			break
		}
		revertFiles, err := fetchOwn()
		if err != nil {
			result.RevertVerification = "message-only"
			break
		}
		ce.Stats.RevertVerification.Add(1)
		startRevertVerify2 := time.Now()
		revertedFiles, err := ce.client.GetCommitFiles(ctx, org, repo, revertedSHA)
		ce.Stats.RevertVerificationNanos.Add(time.Since(startRevertVerify2).Nanoseconds())
		if err != nil {
			result.RevertVerification = "message-only"
			break
		}
		if IsCleanRevertDiff(revertFiles, revertedFiles) {
			result.IsCleanRevert = true
			result.RevertVerification = "diff-verified"
		} else {
			result.RevertVerification = "diff-mismatch"
		}
	}

	// --- Merge classification ---
	// See ClassifyMerge for the detection rule. No extra API call needed:
	// parent count, message, committer, and signature verification are all
	// in the commit detail we fetched at the start of enrichOneCommit.
	mk := ClassifyMerge(parentCount, result.Commit.Message, result.Commit.CommitterLogin, result.Commit.IsVerified)
	result.MergeVerification = MergeKindVerification(mk)
	if mk == CleanMerge {
		result.IsCleanMerge = true
	}
}
