package github

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

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
	CommitDetail       atomic.Int64
	CommitPRs          atomic.Int64
	PRDetail           atomic.Int64
	Reviews            atomic.Int64
	CheckRuns          atomic.Int64
	PRCommits          atomic.Int64
	RevertVerification atomic.Int64 // GetCommitFiles calls made for clean-revert diff check
	CacheHits          atomic.Int64
	DBHits             atomic.Int64
}

// Total returns the total number of API requests made.
func (s *APIStats) Total() int64 {
	return s.CommitDetail.Load() + s.CommitPRs.Load() + s.PRDetail.Load() +
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
	client *Client
	dbCache EnrichmentCache
	Stats  APIStats

	mu              sync.Mutex
	prCache         map[string]*model.PullRequest
	reviewCache     map[string][]model.Review
	checkRunCache   map[string][]model.CheckRun
	prCommitCache   map[string][]model.Commit
	commitPRCache   map[string][]model.PullRequest // sha → PRs (from API or reverse index)
	mergeCommitIdx  map[string][]int               // merge_commit_sha → PR numbers
}

// NewCachingEnricher creates a new caching enricher. If dbCache is non-nil,
// it is consulted for previously-synced data before making API calls.
func NewCachingEnricher(client *Client, dbCache EnrichmentCache) *CachingEnricher {
	return &CachingEnricher{
		client:         client,
		dbCache:        dbCache,
		prCache:        make(map[string]*model.PullRequest),
		reviewCache:    make(map[string][]model.Review),
		checkRunCache:  make(map[string][]model.CheckRun),
		prCommitCache:  make(map[string][]model.Commit),
		commitPRCache:  make(map[string][]model.PullRequest),
		mergeCommitIdx: make(map[string][]int),
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
	ce.Stats.CommitDetail.Add(1)
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
	prs, err := ce.client.ListCommitPullRequests(ctx, org, repo, sha)
	if err != nil {
		return nil, err
	}

	ce.mu.Lock()
	ce.commitPRCache[cacheKey] = prs
	ce.mu.Unlock()
	return prs, nil
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
	pr, err := ce.client.GetPullRequest(ctx, org, repo, number)
	if err != nil {
		return nil, err
	}

	ce.mu.Lock()
	ce.prCache[key] = pr
	ce.indexPR(pr)
	ce.mu.Unlock()
	return pr, nil
}

// isPRMerged returns true when the PR is known to be merged from the
// in-memory cache or DB. The merged state is the freeze trigger: reviews,
// check runs, and PR-branch commits attached to a merged PR are
// immutable, so a DB-empty result is the truth — no API fall-through.
//
// The check is best-effort: a DB error or a missing row falls through to
// the API (returns false), preserving the pre-freeze behaviour for any
// PR we haven't yet seen.
func (ce *CachingEnricher) isPRMerged(ctx context.Context, org, repo string, prNumber int) bool {
	key := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)
	ce.mu.Lock()
	pr, ok := ce.prCache[key]
	ce.mu.Unlock()
	if ok && pr != nil {
		return pr.Merged
	}
	if ce.dbCache == nil {
		return false
	}
	pr, err := ce.dbCache.GetPullRequest(ctx, org, repo, prNumber)
	if err != nil || pr == nil {
		return false
	}
	return pr.Merged
}

func (ce *CachingEnricher) getReviews(ctx context.Context, org, repo string, prNumber int) ([]model.Review, error) {
	key := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)

	ce.mu.Lock()
	if cached, ok := ce.reviewCache[key]; ok {
		ce.mu.Unlock()
		ce.Stats.CacheHits.Add(1)
		return cached, nil
	}
	ce.mu.Unlock()

	// DB fallback. When the PR is merged (frozen), a zero-row result is
	// authoritative — reviews on a merged PR don't change — so we cache
	// the empty slice and skip the API. Open PRs still hit the API on
	// DB-empty so newly-added reviews are discovered.
	if ce.dbCache != nil {
		reviews, err := ce.dbCache.GetReviewsForPR(ctx, org, repo, prNumber)
		if err == nil && (len(reviews) > 0 || ce.isPRMerged(ctx, org, repo, prNumber)) {
			ce.mu.Lock()
			ce.reviewCache[key] = reviews
			ce.mu.Unlock()
			ce.Stats.DBHits.Add(1)
			return reviews, nil
		}
	}

	ce.Stats.Reviews.Add(1)
	reviews, err := ce.client.ListReviews(ctx, org, repo, prNumber)
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

	// DB fallback. Merged-PR freeze trusts a zero-row result.
	if ce.dbCache != nil {
		runs, err := ce.dbCache.GetCheckRunsForCommit(ctx, org, repo, ref)
		if err == nil && (len(runs) > 0 || ce.isPRMerged(ctx, org, repo, prNumber)) {
			ce.mu.Lock()
			ce.checkRunCache[key] = runs
			ce.mu.Unlock()
			ce.Stats.DBHits.Add(1)
			return runs, nil
		}
	}

	ce.Stats.CheckRuns.Add(1)
	runs, err := ce.client.ListCheckRunsForRef(ctx, org, repo, ref)
	if err != nil {
		return nil, err
	}

	ce.mu.Lock()
	ce.checkRunCache[key] = runs
	ce.mu.Unlock()
	return runs, nil
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

	// DB fallback. Merged-PR freeze trusts a zero-row result; in
	// practice every real PR has at least one branch commit, so this
	// branch only matters for empty PRs that somehow got merged.
	if ce.dbCache != nil {
		commits, err := ce.dbCache.GetCommitsForPR(ctx, org, repo, prNumber)
		if err == nil && (len(commits) > 0 || ce.isPRMerged(ctx, org, repo, prNumber)) {
			ce.mu.Lock()
			ce.prCommitCache[key] = commits
			ce.mu.Unlock()
			ce.Stats.DBHits.Add(1)
			return commits, nil
		}
	}

	ce.Stats.PRCommits.Add(1)
	commits, err := ce.client.ListPRCommits(ctx, org, repo, prNumber)
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
		ownFiles, ownFilesErr = ce.client.GetCommitFiles(ctx, org, repo, sha)
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
		revertedFiles, err := ce.client.GetCommitFiles(ctx, org, repo, revertedSHA)
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
	result.MergeVerification = mergeKindVerification(mk)
	if mk == CleanMerge {
		result.IsCleanMerge = true
	}
}
