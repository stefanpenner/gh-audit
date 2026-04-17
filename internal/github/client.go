package github

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v72/github"
	"github.com/stefanpenner/gh-audit/internal/model"
)

var coAuthorRe = regexp.MustCompile(`(?i)co-authored-by:\s*(.+?)\s*<([^>]+)>`)

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

// ListOrgRepos returns all repositories in the given org, paginating with 100 per page.
func (c *Client) ListOrgRepos(ctx context.Context, org string) ([]model.RepoInfo, error) {
	var allRepos []model.RepoInfo
	opts := &gogithub.RepositoryListByOrgOptions{
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}

	for {
		// Pick a fresh token for each page to distribute load.
		gh, err := c.ghClient(ctx, org, "")
		if err != nil {
			return nil, err
		}

		repos, resp, err := gh.Repositories.ListByOrg(ctx, org, opts)
		if err != nil {
			return nil, fmt.Errorf("listing repos for org %s page %d: %w", org, opts.Page, err)
		}

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
			allRepos = append(allRepos, info)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allRepos, nil
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
		gh, err := c.ghClient(ctx, org, repo)
		if err != nil {
			return nil, err
		}

		commits, resp, err := gh.Repositories.ListCommits(ctx, org, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing commits for %s/%s page %d: %w", org, repo, opts.Page, err)
		}

		for _, rc := range commits {
			commit := model.Commit{
				Org:  org,
				Repo: repo,
				SHA:  rc.GetSHA(),
				Href: rc.GetHTMLURL(),
			}
			if rc.GetAuthor() != nil {
				commit.AuthorLogin = rc.GetAuthor().GetLogin()
			}
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
			}
			commit.ParentCount = len(rc.Parents)
			commit.Branch = branch
			commit.CoAuthors = parseCoAuthors(commit.Message)
			allCommits = append(allCommits, commit)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allCommits, nil
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

	commit := &model.Commit{
		Org:  org,
		Repo: repo,
		SHA:  rc.GetSHA(),
		Href: rc.GetHTMLURL(),
	}
	if rc.GetAuthor() != nil {
		commit.AuthorLogin = rc.GetAuthor().GetLogin()
	}
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
	}
	commit.ParentCount = len(rc.Parents)
	commit.CoAuthors = parseCoAuthors(commit.Message)
	if rc.GetStats() != nil {
		commit.Additions = rc.GetStats().GetAdditions()
		commit.Deletions = rc.GetStats().GetDeletions()
	}

	return commit, nil
}

// ListCommitPullRequests returns merged PRs associated with a commit.
func (c *Client) ListCommitPullRequests(ctx context.Context, org, repo, sha string) ([]model.PullRequest, error) {
	var allPRs []model.PullRequest
	opts := &gogithub.ListOptions{PerPage: 100}

	for {
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
				Org:      org,
				Repo:     repo,
				Number:   pr.GetNumber(),
				Title:    pr.GetTitle(),
				Merged:   true,
				HeadSHA:  pr.GetHead().GetSHA(),
				MergedAt: pr.MergedAt.Time,
				Href:     pr.GetHTMLURL(),
			}
			if pr.GetMergeCommitSHA() != "" {
				p.MergeCommitSHA = pr.GetMergeCommitSHA()
			}
			if pr.GetUser() != nil {
				p.AuthorLogin = pr.GetUser().GetLogin()
			}
			if pr.GetMergedBy() != nil {
				p.MergedByLogin = pr.GetMergedBy().GetLogin()
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
	}
	if pr.GetMergeCommitSHA() != "" {
		p.MergeCommitSHA = pr.GetMergeCommitSHA()
	}
	if pr.GetUser() != nil {
		p.AuthorLogin = pr.GetUser().GetLogin()
	}
	if pr.GetMergedBy() != nil {
		p.MergedByLogin = pr.GetMergedBy().GetLogin()
	}
	if pr.MergedAt != nil {
		p.MergedAt = pr.MergedAt.Time
	}
	return p, nil
}

// EnrichCommits fetches PRs, reviews, and check runs for a batch of commits via REST.
// Each commit triggers: GET commit (stats), GET commit PRs, GET PR detail, GET reviews per PR, GET check runs per PR head.
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
		seenCheckRef := make(map[string]bool)

		for j, pr := range prs {
			// The /commits/{sha}/pulls endpoint omits merged_by — fetch full PR detail.
			fullPR, err := c.GetPullRequest(ctx, org, repo, pr.Number)
			if err != nil {
				return nil, fmt.Errorf("commit %s PR #%d detail: %w", sha[:12], pr.Number, err)
			}
			prs[j].MergedByLogin = fullPR.MergedByLogin
			if fullPR.HeadSHA != "" {
				prs[j].HeadSHA = fullPR.HeadSHA
			}

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
		}

		results[i] = model.EnrichmentResult{
			Commit:    *detail,
			PRs:       prs,
			Reviews:   allReviews,
			CheckRuns: allCheckRuns,
		}
	}

	return results, nil
}

// parseCoAuthors extracts co-authors from "Co-authored-by" trailers in commit messages.
func parseCoAuthors(message string) []model.CoAuthor {
	if !strings.Contains(strings.ToLower(message), "co-authored-by") {
		return nil
	}
	matches := coAuthorRe.FindAllStringSubmatch(message, -1)
	if len(matches) == 0 {
		return nil
	}
	coAuthors := make([]model.CoAuthor, 0, len(matches))
	for _, m := range matches {
		coAuthors = append(coAuthors, model.CoAuthor{
			Name:  strings.TrimSpace(m[1]),
			Email: strings.TrimSpace(m[2]),
		})
	}
	return coAuthors
}
