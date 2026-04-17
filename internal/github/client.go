package github

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	gogithub "github.com/google/go-github/v72/github"
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
			if rc.GetCommit() != nil {
				commit.Message = rc.GetCommit().GetMessage()
				if rc.GetCommit().GetAuthor() != nil {
					commit.AuthorEmail = rc.GetCommit().GetAuthor().GetEmail()
					commit.CommittedAt = rc.GetCommit().GetAuthor().GetDate().Time
				}
			}
			commit.ParentCount = len(rc.Parents)
			commit.Branch = branch
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
	if rc.GetCommit() != nil {
		commit.Message = rc.GetCommit().GetMessage()
		if rc.GetCommit().GetAuthor() != nil {
			commit.AuthorEmail = rc.GetCommit().GetAuthor().GetEmail()
			commit.CommittedAt = rc.GetCommit().GetAuthor().GetDate().Time
		}
	}
	commit.ParentCount = len(rc.Parents)
	if rc.GetStats() != nil {
		commit.Additions = rc.GetStats().GetAdditions()
		commit.Deletions = rc.GetStats().GetDeletions()
	}

	return commit, nil
}
