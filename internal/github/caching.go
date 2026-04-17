package github

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// APIStats tracks API request counts by endpoint.
type APIStats struct {
	CommitDetail atomic.Int64
	CommitPRs    atomic.Int64
	PRDetail     atomic.Int64
	Reviews      atomic.Int64
	CheckRuns    atomic.Int64
	PRCommits    atomic.Int64
	CacheHits    atomic.Int64
	DBHits       atomic.Int64
}

// Total returns the total number of API requests made.
func (s *APIStats) Total() int64 {
	return s.CommitDetail.Load() + s.CommitPRs.Load() + s.PRDetail.Load() +
		s.Reviews.Load() + s.CheckRuns.Load() + s.PRCommits.Load()
}

// EnrichmentCache provides read access to previously-synced enrichment data.
// Implemented by *db.DB. Merged PR data is immutable, so DB results are
// always valid and eliminate the need for HTTP-level ETag caching.
type EnrichmentCache interface {
	GetPRsForCommit(ctx context.Context, org, repo, sha string) ([]model.PullRequest, error)
	GetReviewsForPR(ctx context.Context, org, repo string, prNumber int) ([]model.Review, error)
	GetCheckRunsForCommit(ctx context.Context, org, repo, sha string) ([]model.CheckRun, error)
	GetCommitsForPR(ctx context.Context, org, repo string, prNumber int) ([]model.Commit, error)
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

func (ce *CachingEnricher) getReviews(ctx context.Context, org, repo string, prNumber int) ([]model.Review, error) {
	key := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)

	ce.mu.Lock()
	if cached, ok := ce.reviewCache[key]; ok {
		ce.mu.Unlock()
		ce.Stats.CacheHits.Add(1)
		return cached, nil
	}
	ce.mu.Unlock()

	// DB fallback
	if ce.dbCache != nil {
		reviews, err := ce.dbCache.GetReviewsForPR(ctx, org, repo, prNumber)
		if err == nil && len(reviews) > 0 {
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

func (ce *CachingEnricher) getCheckRuns(ctx context.Context, org, repo, ref string) ([]model.CheckRun, error) {
	key := fmt.Sprintf("%s/%s/%s", org, repo, ref)

	ce.mu.Lock()
	if cached, ok := ce.checkRunCache[key]; ok {
		ce.mu.Unlock()
		ce.Stats.CacheHits.Add(1)
		return cached, nil
	}
	ce.mu.Unlock()

	// DB fallback
	if ce.dbCache != nil {
		runs, err := ce.dbCache.GetCheckRunsForCommit(ctx, org, repo, ref)
		if err == nil && len(runs) > 0 {
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

	// DB fallback
	if ce.dbCache != nil {
		commits, err := ce.dbCache.GetCommitsForPR(ctx, org, repo, prNumber)
		if err == nil && len(commits) > 0 {
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
func (ce *CachingEnricher) EnrichCommits(ctx context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	results := make([]model.EnrichmentResult, len(shas))

	for i, sha := range shas {
		ce.Stats.CommitDetail.Add(1)
		detail, err := ce.client.GetCommitDetail(ctx, org, repo, sha)
		if err != nil {
			return nil, fmt.Errorf("commit %s: %w", sha[:12], err)
		}

		prs, err := ce.getPRsForCommit(ctx, org, repo, sha)
		if err != nil {
			return nil, fmt.Errorf("commit %s PRs: %w", sha[:12], err)
		}

		var allReviews []model.Review
		var allCheckRuns []model.CheckRun
		prBranchCommits := make(map[int][]model.Commit)
		seenCheckRef := make(map[string]bool)

		for j, pr := range prs {
			fullPR, err := ce.getPR(ctx, org, repo, pr.Number)
			if err != nil {
				return nil, fmt.Errorf("commit %s PR #%d detail: %w", sha[:12], pr.Number, err)
			}
			prs[j].MergedByLogin = fullPR.MergedByLogin
			if fullPR.HeadSHA != "" {
				prs[j].HeadSHA = fullPR.HeadSHA
			}
			prs[j].HeadBranch = fullPR.HeadBranch

			reviews, err := ce.getReviews(ctx, org, repo, pr.Number)
			if err != nil {
				return nil, fmt.Errorf("commit %s PR #%d reviews: %w", sha[:12], pr.Number, err)
			}
			allReviews = append(allReviews, reviews...)

			if prs[j].HeadSHA != "" && !seenCheckRef[prs[j].HeadSHA] {
				seenCheckRef[prs[j].HeadSHA] = true
				runs, err := ce.getCheckRuns(ctx, org, repo, prs[j].HeadSHA)
				if err != nil {
					return nil, fmt.Errorf("commit %s PR #%d check runs: %w", sha[:12], pr.Number, err)
				}
				allCheckRuns = append(allCheckRuns, runs...)
			}

			branchCommits, err := ce.getPRCommits(ctx, org, repo, pr.Number)
			if err != nil {
				return nil, fmt.Errorf("commit %s PR #%d pr-commits: %w", sha[:12], pr.Number, err)
			}
			prBranchCommits[pr.Number] = branchCommits
		}

		results[i] = model.EnrichmentResult{
			Commit:          *detail,
			PRs:             prs,
			Reviews:         allReviews,
			CheckRuns:       allCheckRuns,
			PRBranchCommits: prBranchCommits,
		}
	}

	return results, nil
}
