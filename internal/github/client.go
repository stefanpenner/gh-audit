package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v72/github"
	"golang.org/x/sync/errgroup"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// Client wraps the GitHub REST API with token-pool-aware authentication.
type Client struct {
	pool   *TokenPool
	logger *slog.Logger
}

// NewClient creates a new REST API client.
func NewClient(pool *TokenPool, logger *slog.Logger) *Client {
	return &Client{
		pool:   pool,
		logger: logger,
	}
}

// ghClient picks a token and creates a go-github client for a single API call.
func (c *Client) ghClient(ctx context.Context, org, repo string) (*gogithub.Client, error) {
	httpClient, err := c.pool.Pick(ctx, org, repo)
	if err != nil {
		return nil, fmt.Errorf("picking token for %s/%s: %w", org, repo, err)
	}
	return gogithub.NewClient(httpClient), nil
}

// listOrgReposPagePerPage caps the per-page response size at GitHub's
// maximum. Larger pages mean fewer total round-trips (n=370 → 37 with
// per_page=100 vs page_size=10).
const listOrgReposPagePerPage = 100

// listOrgReposParallelism bounds concurrent page fetches once total
// page count is known. Each page picks a fresh token from the pool, so
// 8 parallel fetches × 12 tokens = ~24% of pool concurrency for org
// enumeration alone — well below the 100-per-token secondary cap and
// leaves bandwidth for whatever else is in flight.
const listOrgReposParallelism = 8

// ListOrgRepos returns all repositories in the given org. The first
// page is fetched serially to discover the total page count from
// GitHub's `Link: rel="last"` header (exposed by go-github as
// resp.LastPage); the remaining pages fan out concurrently. On a
// 37k-repo org this drops a 60-90s serial enumeration to <10s.
func (c *Client) ListOrgRepos(ctx context.Context, org string) ([]model.RepoInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gh, err := c.ghClient(ctx, org, "")
	if err != nil {
		return nil, err
	}

	firstOpts := &gogithub.RepositoryListByOrgOptions{
		ListOptions: gogithub.ListOptions{PerPage: listOrgReposPagePerPage, Page: 1},
	}
	firstRepos, resp, err := gh.Repositories.ListByOrg(ctx, org, firstOpts)
	if err != nil {
		return nil, fmt.Errorf("listing repos for org %s page 1: %w", org, err)
	}

	totalPages := resp.LastPage
	if totalPages < 1 {
		totalPages = 1
	}

	pages := make([][]model.RepoInfo, totalPages)
	pages[0] = convertRepoList(org, firstRepos)

	if totalPages > 1 {
		eg, gctx := errgroup.WithContext(ctx)
		eg.SetLimit(listOrgReposParallelism)
		for p := 2; p <= totalPages; p++ {
			p := p
			eg.Go(func() error {
				if err := gctx.Err(); err != nil {
					return err
				}
				gh, err := c.ghClient(gctx, org, "")
				if err != nil {
					return err
				}
				opts := &gogithub.RepositoryListByOrgOptions{
					ListOptions: gogithub.ListOptions{PerPage: listOrgReposPagePerPage, Page: p},
				}
				repos, _, err := gh.Repositories.ListByOrg(gctx, org, opts)
				if err != nil {
					return fmt.Errorf("listing repos for org %s page %d: %w", org, p, err)
				}
				pages[p-1] = convertRepoList(org, repos)
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, err
		}
	}

	var all []model.RepoInfo
	for _, batch := range pages {
		all = append(all, batch...)
	}
	return all, nil
}

// convertRepoList maps go-github repository structs into gh-audit's
// internal model. Pulled out so the parallel fetch goroutines don't
// need to duplicate field plumbing.
func convertRepoList(org string, repos []*gogithub.Repository) []model.RepoInfo {
	out := make([]model.RepoInfo, 0, len(repos))
	for _, r := range repos {
		info := model.RepoInfo{
			Org:      org,
			Name:     r.GetName(),
			FullName: r.GetFullName(),
			Archived: r.GetArchived(),
		}
		if r.DefaultBranch != nil {
			info.DefaultBranch = r.GetDefaultBranch()
		}
		out = append(out, info)
	}
	return out
}

// GetRepo returns metadata for a single repository.
func (c *Client) GetRepo(ctx context.Context, org, repo string) (model.RepoInfo, error) {
	gh, err := c.ghClient(ctx, org, repo)
	if err != nil {
		return model.RepoInfo{}, err
	}

	r, _, err := gh.Repositories.Get(ctx, org, repo)
	if err != nil {
		return model.RepoInfo{}, fmt.Errorf("getting repo %s/%s: %w", org, repo, err)
	}

	info := model.RepoInfo{
		Org:      org,
		Name:     r.GetName(),
		FullName: r.GetFullName(),
		Archived: r.GetArchived(),
	}
	if r.DefaultBranch != nil {
		info.DefaultBranch = r.GetDefaultBranch()
	}
	return info, nil
}

// ListCommits returns all commits on the specified branch within the time range, paginating.
// The branch parameter is passed as the SHA field in the GitHub API to filter by branch.
func (c *Client) ListCommits(ctx context.Context, org, repo, branch string, since, until time.Time) ([]model.Commit, error) {
	var allCommits []model.Commit
	opts := &gogithub.CommitsListOptions{
		SHA:         branch,
		Since:       since,
		Until:       until,
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}

		commits, resp, err := gh.Repositories.ListCommits(ctx, org, repo, opts)
		if err != nil {
			// GitHub answers GET /commits on an empty repository with
			// 409 Conflict ("Git Repository is empty."). That is zero
			// commits, not a sync failure.
			var ghErr *gogithub.ErrorResponse
			if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusConflict {
				return nil, nil
			}
			return nil, fmt.Errorf("listing commits for %s/%s page %d: %w", org, repo, opts.Page, err)
		}

		for _, rc := range commits {
			allCommits = append(allCommits, c.convertRepoCommit(org, repo, branch, rc))
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allCommits, nil
}

// convertRepoCommit maps a go-github RepositoryCommit (from the list,
// compare, or detail endpoints) into the internal model. Shared so every
// ingestion path produces identically-shaped rows.
func (c *Client) convertRepoCommit(org, repo, branch string, rc *gogithub.RepositoryCommit) model.Commit {
	commit := model.Commit{
		Org:  org,
		Repo: repo,
		SHA:  rc.GetSHA(),
		Href: rc.GetHTMLURL(),
	}
	c.resolveAuthor(&commit, rc)
	if rc.GetCommitter() != nil {
		commit.CommitterLogin = rc.GetCommitter().GetLogin()
	}
	if rc.GetCommit() != nil {
		commit.Message = rc.GetCommit().GetMessage()
		if rc.GetCommit().GetAuthor() != nil {
			commit.AuthorEmail = rc.GetCommit().GetAuthor().GetEmail()
		}
		if rc.GetCommit().GetCommitter() != nil {
			commit.CommittedAt = rc.GetCommit().GetCommitter().GetDate().Time
		}
		if rc.GetCommit().GetVerification() != nil {
			commit.IsVerified = rc.GetCommit().GetVerification().GetVerified()
		}
	}
	commit.ParentCount = len(rc.Parents)
	for _, p := range rc.Parents {
		if sha := p.GetSHA(); sha != "" {
			commit.ParentSHAs = append(commit.ParentSHAs, sha)
		}
	}
	commit.Branch = branch
	commit.CoAuthors = model.ParseCoAuthors(commit.Message)
	return commit
}

// GetBranchHead returns the branch's current tip SHA.
func (c *Client) GetBranchHead(ctx context.Context, org, repo, branch string) (string, error) {
	gh, err := c.ghClient(ctx, org, repo)
	if err != nil {
		return "", err
	}
	b, _, err := gh.Repositories.GetBranch(ctx, org, repo, branch, 0)
	if err != nil {
		return "", fmt.Errorf("getting branch head %s/%s@%s: %w", org, repo, branch, err)
	}
	sha := b.GetCommit().GetSHA()
	if sha == "" {
		return "", fmt.Errorf("branch %s/%s@%s has no tip commit", org, repo, branch)
	}
	return sha, nil
}

// ErrCompareUnavailable signals that base...head comparison cannot serve an
// incremental fetch: the base SHA is gone (force-push / history rewrite,
// HTTP 404) or the range exceeds the compare API's commit ceiling. Callers
// fall back to the date-window commit listing.
var ErrCompareUnavailable = errors.New("compare unavailable for incremental fetch")

// compareCommitsCeiling is GitHub's documented maximum for the commits list
// of a compare response. Ranges beyond it must use the list endpoint.
const compareCommitsCeiling = 250

// CompareCommits returns the commits reachable from head but not from base
// — the graph difference, computed by GitHub's compare API. Unlike the
// date-filtered list endpoint, the result is immune to committer-date
// backdating, which makes it the trustworthy primitive for incremental
// sync. The branch parameter only labels the returned commits.
func (c *Client) CompareCommits(ctx context.Context, org, repo, base, head, branch string) ([]model.Commit, error) {
	var all []model.Commit
	opts := &gogithub.ListOptions{PerPage: 100}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}
		comp, resp, err := gh.Repositories.CompareCommits(ctx, org, repo, base, head, opts)
		if err != nil {
			var ghErr *gogithub.ErrorResponse
			if errors.As(err, &ghErr) && ghErr.Response != nil &&
				(ghErr.Response.StatusCode == http.StatusNotFound || ghErr.Response.StatusCode == http.StatusUnprocessableEntity) {
				return nil, fmt.Errorf("%w: %s/%s %s...%s: %v", ErrCompareUnavailable, org, repo, base, head, err)
			}
			return nil, fmt.Errorf("comparing %s/%s %s...%s page %d: %w", org, repo, base, head, opts.Page, err)
		}
		if comp.GetTotalCommits() > compareCommitsCeiling {
			return nil, fmt.Errorf("%w: %s/%s %s...%s spans %d commits (> %d ceiling)",
				ErrCompareUnavailable, org, repo, base, head, comp.GetTotalCommits(), compareCommitsCeiling)
		}
		for _, rc := range comp.Commits {
			all = append(all, c.convertRepoCommit(org, repo, branch, rc))
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// GetCommitDetail fetches a single commit with addition/deletion stats.
func (c *Client) GetCommitDetail(ctx context.Context, org, repo, sha string) (*model.Commit, error) {
	gh, err := c.ghClient(ctx, org, repo)
	if err != nil {
		return nil, err
	}

	rc, _, err := gh.Repositories.GetCommit(ctx, org, repo, sha, nil)
	if err != nil {
		return nil, fmt.Errorf("getting commit detail %s/%s@%s: %w", org, repo, sha, err)
	}

	converted := c.convertRepoCommit(org, repo, "", rc)
	commit := &converted
	commit.FilesChanged = len(rc.Files)
	commit.StatsVerified = true
	if rc.GetStats() != nil {
		commit.Additions = rc.GetStats().GetAdditions()
		commit.Deletions = rc.GetStats().GetDeletions()
	}

	return commit, nil
}

// commitFilesCeiling is GitHub's hard cap on the files list of
// GET /commits/{sha}: at most 300 files per page and 3000 files total,
// beyond which the list is silently truncated.
const commitFilesCeiling = 3000

// ErrCommitFilesTruncated signals that a commit touches at least
// commitFilesCeiling files, so the returned list may be incomplete.
// Callers doing diff verification must treat the commit as unverifiable
// (e.g. revert_verification="message-only"), never as verified.
var ErrCommitFilesTruncated = errors.New("commit file list truncated at GitHub's 3000-file ceiling")

// GetCommitFiles fetches the per-file patch list for a commit, fully
// paginated. Used by clean-revert verification to compare the revert's diff
// against the diff of the commit it claims to revert.
//
// Returns ErrCommitFilesTruncated (alongside the partial list) when the
// commit reaches GitHub's 3000-file ceiling — the list cannot be trusted
// for verification beyond that point.
func (c *Client) GetCommitFiles(ctx context.Context, org, repo, sha string) ([]model.FileDiff, error) {
	var files []model.FileDiff
	opts := &gogithub.ListOptions{PerPage: 300}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}

		rc, resp, err := gh.Repositories.GetCommit(ctx, org, repo, sha, opts)
		if err != nil {
			return nil, fmt.Errorf("getting commit files %s/%s@%s page %d: %w", org, repo, sha, opts.Page, err)
		}

		for _, f := range rc.Files {
			files = append(files, model.FileDiff{
				Filename:  f.GetFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
				Patch:     f.GetPatch(),
			})
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if len(files) >= commitFilesCeiling {
		return files, ErrCommitFilesTruncated
	}
	return files, nil
}

// FindMergingPR returns the PR whose merge_commit_sha equals targetSHA, by
// enumerating closed PRs whose merge occurred within a time window around
// committedAt. This is the canonical way to answer "which PR delivered this
// commit to master" without relying on the commit message or the
// /commits/:sha/pulls endpoint (which returns *every* PR whose branch
// contains the commit — often attributing a squash-merged commit to a later,
// unrelated PR that happens to have the commit in its branch history).
//
// The caller supplies the base branches that count as delivery targets
// (typically the audit_branches list: master, main, release/*, HF_BF_*, …).
// windowBefore / windowAfter bound how far back/forward we scan closed PRs.
//
// Returns (nil, nil) when no PR in the window matches — the commit may have
// been pushed directly, merged before the window opened, or merged onto a
// branch we don't consider.
func (c *Client) FindMergingPR(
	ctx context.Context,
	org, repo, targetSHA string,
	committedAt time.Time,
	baseBranches []string,
	windowBefore, windowAfter time.Duration,
) (*model.PullRequest, error) {
	windowStart := committedAt.Add(-windowBefore)
	windowEnd := committedAt.Add(windowAfter)
	targetSHA = strings.ToLower(targetSHA)

	for _, base := range baseBranches {
		pr, err := c.findMergingPROnBase(ctx, org, repo, targetSHA, base, windowStart, windowEnd)
		if err != nil {
			return nil, err
		}
		if pr != nil {
			return pr, nil
		}
	}
	return nil, nil
}

// ListClosedMergedPRs paginates GitHub's PR list for (base, state=closed)
// newest-first, invoking `cb` for each merged PR whose merged_at falls in
// [windowStart, windowEnd]. Pagination stops when a page's oldest
// updated_at drops below windowStart — older PRs can't have matching
// merged_at values. Intended for bulk backfill use cases that want to
// index a repo's merged history once and then match many SHAs locally.
func (c *Client) ListClosedMergedPRs(
	ctx context.Context,
	org, repo, base string,
	windowStart, windowEnd time.Time,
	cb func(*model.PullRequest),
) error {
	opts := &gogithub.PullRequestListOptions{
		State:       "closed",
		Base:        base,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return err
		}
		prs, resp, err := gh.PullRequests.List(ctx, org, repo, opts)
		if err != nil {
			return fmt.Errorf("listing PRs %s/%s base=%s page %d: %w", org, repo, base, opts.Page, err)
		}
		for _, pr := range prs {
			if pr.MergedAt == nil {
				continue
			}
			merged := pr.GetMergedAt().Time
			if merged.Before(windowStart) || merged.After(windowEnd) {
				continue
			}
			cb(buildPRModel(org, repo, pr))
		}
		if resp.NextPage == 0 {
			break
		}
		if len(prs) > 0 {
			oldestUpdated := prs[len(prs)-1].GetUpdatedAt().Time
			if oldestUpdated.Before(windowStart) {
				break
			}
		}
		opts.Page = resp.NextPage
	}
	return nil
}

// findMergingPROnBase scans closed PRs on a single base branch, newest-first,
// until it either finds a merge_commit_sha match or walks past windowStart.
// Branch patterns with wildcards (e.g. "release/*") are passed through to
// GitHub's base filter verbatim — the API treats an unknown base as "no
// match" rather than erroring, so wildcards get quietly ignored and we fall
// back to scanning the default branch via subsequent iterations. Concrete
// branches (master, main, specific release branch names) work reliably.
func (c *Client) findMergingPROnBase(
	ctx context.Context,
	org, repo, targetSHA, base string,
	windowStart, windowEnd time.Time,
) (*model.PullRequest, error) {
	opts := &gogithub.PullRequestListOptions{
		State:       "closed",
		Base:        base,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}
		prs, resp, err := gh.PullRequests.List(ctx, org, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing PRs %s/%s base=%s page %d: %w", org, repo, base, opts.Page, err)
		}
		for _, pr := range prs {
			if pr.MergedAt == nil {
				continue
			}
			// Page is updated-desc but merged_at can diverge from updated_at
			// (e.g. someone edits the PR title years later). Scan every row
			// in the page; use updated_at only as the pagination stop signal.
			merged := pr.GetMergedAt().Time
			if merged.Before(windowStart) || merged.After(windowEnd) {
				continue
			}
			if strings.EqualFold(pr.GetMergeCommitSHA(), targetSHA) {
				return buildPRModel(org, repo, pr), nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		// Stop scanning when the page's youngest updated_at drops below the
		// window — older PRs can't possibly match, and we'd just burn API
		// budget paginating further.
		if len(prs) > 0 {
			oldestUpdated := prs[len(prs)-1].GetUpdatedAt().Time
			if oldestUpdated.Before(windowStart) {
				break
			}
		}
		opts.Page = resp.NextPage
	}
	return nil, nil
}

// buildPRModel converts a go-github PullRequest into our model shape. Kept
// package-private because it duplicates the conversion logic in
// ListCommitPullRequests; a future refactor could consolidate.
func buildPRModel(org, repo string, pr *gogithub.PullRequest) *model.PullRequest {
	p := &model.PullRequest{
		Org:        org,
		Repo:       repo,
		Number:     pr.GetNumber(),
		Title:      pr.GetTitle(),
		Merged:     true,
		HeadSHA:    pr.GetHead().GetSHA(),
		HeadBranch: pr.GetHead().GetRef(),
		Href:       pr.GetHTMLURL(),
	}
	if pr.MergedAt != nil {
		p.MergedAt = pr.MergedAt.Time
	}
	if pr.GetMergeCommitSHA() != "" {
		p.MergeCommitSHA = pr.GetMergeCommitSHA()
	}
	if pr.GetUser() != nil {
		p.AuthorLogin = pr.GetUser().GetLogin()
		p.AuthorID = pr.GetUser().GetID()
	}
	if pr.GetMergedBy() != nil {
		p.MergedByLogin = pr.GetMergedBy().GetLogin()
		p.MergedByID = pr.GetMergedBy().GetID()
	}
	return p
}

// ListCommitPullRequests returns merged PRs associated with a commit.
func (c *Client) ListCommitPullRequests(ctx context.Context, org, repo, sha string) ([]model.PullRequest, error) {
	var allPRs []model.PullRequest
	opts := &gogithub.ListOptions{PerPage: 100}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}

		prs, resp, err := gh.PullRequests.ListPullRequestsWithCommit(ctx, org, repo, sha, opts)
		if err != nil {
			return nil, fmt.Errorf("listing PRs for commit %s/%s@%s page %d: %w", org, repo, sha, opts.Page, err)
		}

		for _, pr := range prs {
			// The /commits/{sha}/pulls endpoint does not populate the
			// "merged" boolean — it's always null. Use merged_at instead.
			if pr.MergedAt == nil {
				continue
			}
			p := model.PullRequest{
				Org:        org,
				Repo:       repo,
				Number:     pr.GetNumber(),
				Title:      pr.GetTitle(),
				Merged:     true,
				HeadSHA:    pr.GetHead().GetSHA(),
				HeadBranch: pr.GetHead().GetRef(),
				MergedAt:   pr.MergedAt.Time,
				Href:       pr.GetHTMLURL(),
			}
			if pr.GetMergeCommitSHA() != "" {
				p.MergeCommitSHA = pr.GetMergeCommitSHA()
			}
			if pr.GetUser() != nil {
				p.AuthorLogin = pr.GetUser().GetLogin()
				p.AuthorID = pr.GetUser().GetID()
			}
			if pr.GetMergedBy() != nil {
				p.MergedByLogin = pr.GetMergedBy().GetLogin()
				p.MergedByID = pr.GetMergedBy().GetID()
			}
			allPRs = append(allPRs, p)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allPRs, nil
}

// ListReviews returns all reviews for a PR, fully paginated.
func (c *Client) ListReviews(ctx context.Context, org, repo string, prNumber int) ([]model.Review, error) {
	var allReviews []model.Review
	opts := &gogithub.ListOptions{PerPage: 100}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}

		reviews, resp, err := gh.PullRequests.ListReviews(ctx, org, repo, prNumber, opts)
		if err != nil {
			return nil, fmt.Errorf("listing reviews for %s/%s#%d page %d: %w", org, repo, prNumber, opts.Page, err)
		}

		for _, r := range reviews {
			review := model.Review{
				Org:      org,
				Repo:     repo,
				PRNumber: prNumber,
				ReviewID: r.GetID(),
				State:    r.GetState(),
			}
			if r.User != nil {
				review.ReviewerLogin = r.User.GetLogin()
				review.ReviewerID = r.User.GetID()
			}
			if r.CommitID != nil {
				review.CommitID = r.GetCommitID()
			}
			if r.SubmittedAt != nil {
				review.SubmittedAt = r.SubmittedAt.Time
			}
			if r.HTMLURL != nil {
				review.Href = r.GetHTMLURL()
			}
			allReviews = append(allReviews, review)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allReviews, nil
}

// ListCheckRunsForRef returns all check runs for a git ref (SHA), fully paginated.
func (c *Client) ListCheckRunsForRef(ctx context.Context, org, repo, ref string) ([]model.CheckRun, error) {
	var allRuns []model.CheckRun
	opts := &gogithub.ListCheckRunsOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}

		result, resp, err := gh.Checks.ListCheckRunsForRef(ctx, org, repo, ref, opts)
		if err != nil {
			return nil, fmt.Errorf("listing check runs for %s/%s@%s page %d: %w", org, repo, ref, opts.Page, err)
		}

		for _, cr := range result.CheckRuns {
			run := model.CheckRun{
				Org:        org,
				Repo:       repo,
				CommitSHA:  ref,
				CheckRunID: cr.GetID(),
				CheckName:  cr.GetName(),
				Status:     cr.GetStatus(),
				Conclusion: cr.GetConclusion(),
			}
			if cr.CompletedAt != nil {
				run.CompletedAt = cr.CompletedAt.Time
			}
			allRuns = append(allRuns, run)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allRuns, nil
}

// GetPullRequest fetches a single pull request with full details (including merged_by).
func (c *Client) GetPullRequest(ctx context.Context, org, repo string, number int) (*model.PullRequest, error) {
	gh, err := c.ghClient(ctx, org, repo)
	if err != nil {
		return nil, err
	}

	pr, _, err := gh.PullRequests.Get(ctx, org, repo, number)
	if err != nil {
		return nil, fmt.Errorf("getting PR %s/%s#%d: %w", org, repo, number, err)
	}

	p := &model.PullRequest{
		Org:    org,
		Repo:   repo,
		Number: pr.GetNumber(),
		Title:  pr.GetTitle(),
		Merged: pr.GetMerged(),
		Href:   pr.GetHTMLURL(),
	}
	if pr.GetHead() != nil {
		p.HeadSHA = pr.GetHead().GetSHA()
		p.HeadBranch = pr.GetHead().GetRef()
	}
	if pr.GetMergeCommitSHA() != "" {
		p.MergeCommitSHA = pr.GetMergeCommitSHA()
	}
	if pr.GetUser() != nil {
		p.AuthorLogin = pr.GetUser().GetLogin()
		p.AuthorID = pr.GetUser().GetID()
	}
	if pr.GetMergedBy() != nil {
		p.MergedByLogin = pr.GetMergedBy().GetLogin()
		p.MergedByID = pr.GetMergedBy().GetID()
	}
	if pr.MergedAt != nil {
		p.MergedAt = pr.MergedAt.Time
	}
	return p, nil
}

// ListPRCommits returns all commits on a pull request's feature branch as
// regular Commit objects. These are stored in the commits table alongside
// default-branch commits, distinguished by commit_branches entries.
func (c *Client) ListPRCommits(ctx context.Context, org, repo string, prNumber int) ([]model.Commit, error) {
	opts := &gogithub.ListOptions{PerPage: 100}
	var all []model.Commit

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Re-pick a client per page (matching ListCommits/ListReviews):
		// Pick reserves one inFlight slot per request and the transport
		// releases one per response, so reusing a single picked client
		// across pages would drive the token's inFlight negative and
		// corrupt Pick's anti-herding score.
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}
		commits, resp, err := gh.PullRequests.ListCommits(ctx, org, repo, prNumber, opts)
		if err != nil {
			return nil, fmt.Errorf("listing PR commits %s/%s#%d page %d: %w", org, repo, prNumber, opts.Page, err)
		}

		for _, rc := range commits {
			// Full conversion, same as every other ingestion path. A
			// hand-rolled subset here once dropped fields that squash-merged
			// PRs' branch commits carry ONLY via this endpoint (author id,
			// parent SHAs, verification), silently breaking the §1 squash
			// backstop, the §4 refresh carve-out, and merge classification.
			all = append(all, c.convertRepoCommit(org, repo, "", rc))
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return all, nil
}

// ListReviewDismissals resolves WHEN each dismissed review on a PR was
// dismissed and what state it held until then, from the issue-events API
// ("review_dismissed" events). GitHub mutates dismissed reviews in place —
// the review row keeps its original submitted_at — so this event stream
// is the only source of the dismissal time. Keyed by review id.
func (c *Client) ListReviewDismissals(ctx context.Context, org, repo string, prNumber int) (map[int64]model.ReviewDismissal, error) {
	out := make(map[int64]model.ReviewDismissal)
	opts := &gogithub.ListOptions{PerPage: 100}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}
		events, resp, err := gh.Issues.ListIssueEvents(ctx, org, repo, prNumber, opts)
		if err != nil {
			return nil, fmt.Errorf("listing issue events %s/%s#%d page %d: %w", org, repo, prNumber, opts.Page, err)
		}
		for _, ev := range events {
			if ev.GetEvent() != "review_dismissed" || ev.GetDismissedReview() == nil {
				continue
			}
			dr := ev.GetDismissedReview()
			out[dr.GetReviewID()] = model.ReviewDismissal{
				At:            ev.GetCreatedAt().Time,
				OriginalState: dr.GetState(),
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// ListStatusContexts fetches the legacy commit-status API's combined
// status for ref and maps each context to a synthetic CheckRun, so §6's
// required-check evaluation can see CI that reports through /statuses
// (older Jenkins setups) instead of the Checks API.
//
// Mapping: success/failure/error -> status "completed" with the state as
// the conclusion; pending -> "in_progress" with no conclusion (reads as
// a not-yet-concluded run downstream). The combined endpoint already
// returns only the LATEST status per context. CheckRunID is the status
// event id NEGATED so the legacy-status id space can never collide with
// real check-run ids in the check_runs table.
func (c *Client) ListStatusContexts(ctx context.Context, org, repo, ref string) ([]model.CheckRun, error) {
	var out []model.CheckRun
	opts := &gogithub.ListOptions{PerPage: 100}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}
		combined, resp, err := gh.Repositories.GetCombinedStatus(ctx, org, repo, ref, opts)
		if err != nil {
			return nil, fmt.Errorf("combined status for %s/%s@%s page %d: %w", org, repo, ref, opts.Page, err)
		}
		for _, st := range combined.Statuses {
			run := model.CheckRun{
				Org:        org,
				Repo:       repo,
				CommitSHA:  ref,
				CheckRunID: -st.GetID(),
				CheckName:  st.GetContext(),
			}
			switch st.GetState() {
			case "pending":
				run.Status = "in_progress"
			default: // success, failure, error
				run.Status = "completed"
				run.Conclusion = st.GetState()
			}
			if st.UpdatedAt != nil {
				run.CompletedAt = st.UpdatedAt.Time
			}
			out = append(out, run)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// EnrichCommits fetches PRs, reviews, check runs, and PR branch commits for a batch of commits via REST.
// Each commit triggers: GET commit, GET commit PRs, GET PR detail, GET reviews, GET check runs, GET PR commits.
func (c *Client) EnrichCommits(ctx context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	results := make([]model.EnrichmentResult, len(shas))

	for i, sha := range shas {
		detail, err := c.GetCommitDetail(ctx, org, repo, sha)
		if err != nil {
			return nil, fmt.Errorf("commit %s: %w", sha[:12], err)
		}

		prs, err := c.ListCommitPullRequests(ctx, org, repo, sha)
		if err != nil {
			return nil, fmt.Errorf("commit %s PRs: %w", sha[:12], err)
		}

		var allReviews []model.Review
		var allCheckRuns []model.CheckRun
		prBranchCommits := make(map[int][]model.Commit)
		seenCheckRef := make(map[string]bool)

		for j, pr := range prs {
			fullPR, err := c.GetPullRequest(ctx, org, repo, pr.Number)
			if err != nil {
				return nil, fmt.Errorf("commit %s PR #%d detail: %w", sha[:12], pr.Number, err)
			}
			prs[j].MergedByLogin = fullPR.MergedByLogin
			prs[j].MergedByID = fullPR.MergedByID
			if fullPR.HeadSHA != "" {
				prs[j].HeadSHA = fullPR.HeadSHA
			}
			prs[j].HeadBranch = fullPR.HeadBranch

			reviews, err := c.ListReviews(ctx, org, repo, pr.Number)
			if err != nil {
				return nil, fmt.Errorf("commit %s PR #%d reviews: %w", sha[:12], pr.Number, err)
			}
			allReviews = append(allReviews, reviews...)

			if prs[j].HeadSHA != "" && !seenCheckRef[prs[j].HeadSHA] {
				seenCheckRef[prs[j].HeadSHA] = true
				runs, err := c.ListCheckRunsForRef(ctx, org, repo, prs[j].HeadSHA)
				if err != nil {
					return nil, fmt.Errorf("commit %s PR #%d check runs: %w", sha[:12], pr.Number, err)
				}
				allCheckRuns = append(allCheckRuns, runs...)
			}

			branchCommits, err := c.ListPRCommits(ctx, org, repo, pr.Number)
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

// resolveAuthor populates AuthorLogin and AuthorID on the commit if
// GitHub resolved the commit's git-author email to a verified GH user.
// Otherwise it logs a one-line warning with the actionable fix-it text
// and leaves AuthorID == 0 — the audit rules treat that as "non-exempt
// contributor", which is the correct conservative read.
//
// Why not error out:
//
//   - The forgery-resistance contract lives in audit-time matching
//     (audit.go::isExemptCommit matches on id only; a zero ID can never
//     be exempt). Aborting sync at ingest doesn't add security; it
//     just denies coverage.
//   - Real developer commits routinely arrive with laptop-hostname
//     emails (e.g. "user@user-mn4857.linkedin.biz") that GitHub can't
//     bind to an account. They're not exempt anyway (no email path),
//     so a missing AuthorID is fine — they fall through to normal
//     review rules.
//
// The fix-it text in the warning helps operators surface and remediate
// chronic null-author cases (typically a misconfigured bot whose
// commits were intended to be exempted).
func (c *Client) resolveAuthor(commit *model.Commit, rc *gogithub.RepositoryCommit) {
	if rc.GetAuthor() != nil && rc.GetAuthor().GetID() != 0 {
		commit.AuthorLogin = rc.GetAuthor().GetLogin()
		commit.AuthorID = rc.GetAuthor().GetID()
		return
	}
	email := ""
	if rc.GetCommit() != nil && rc.GetCommit().GetAuthor() != nil {
		email = rc.GetCommit().GetAuthor().GetEmail()
	}
	c.logger.Warn("commit has no GitHub-resolved author",
		"org", commit.Org,
		"repo", commit.Repo,
		"sha", commit.SHA,
		"git_author_email", email,
		"fix", "register the email on the matching GitHub account at https://github.com/settings/emails to enable id-based exempt-author matching; otherwise the commit will fall through to the standard review rules",
	)
}
