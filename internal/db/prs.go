package db

import (
	"context"
	"database/sql/driver"
	"fmt"

	"github.com/stefanpenner/gh-audit/internal/model"
)

var prColumns = []string{
	"org", "repo", "number", "title", "merged", "head_sha", "head_branch",
	"merge_commit_sha", "author_login", "merged_by_login", "merged_at", "href",
}

// UpsertPullRequests batch-inserts pull requests using the DuckDB Appender API
// with a staging table for upsert semantics.
func (d *DB) UpsertPullRequests(ctx context.Context, prs []model.PullRequest) error {
	if len(prs) == 0 {
		return nil
	}

	rows := make([][]driver.Value, len(prs))
	for i, pr := range prs {
		rows[i] = []driver.Value{
			pr.Org, pr.Repo, pr.Number, pr.Title, pr.Merged,
			pr.HeadSHA, pr.HeadBranch, pr.MergeCommitSHA, pr.AuthorLogin, pr.MergedByLogin, pr.MergedAt, pr.Href,
		}
	}

	return d.bulkUpsert(ctx, "pull_requests", prColumns, []string{"org", "repo", "number"}, rows)
}

var reviewColumns = []string{
	"org", "repo", "pr_number", "review_id", "reviewer_login",
	"state", "commit_id", "submitted_at", "href",
}

// UpsertReviews batch-inserts reviews using the DuckDB Appender API with a
// staging table for upsert semantics.
func (d *DB) UpsertReviews(ctx context.Context, reviews []model.Review) error {
	if len(reviews) == 0 {
		return nil
	}

	rows := make([][]driver.Value, len(reviews))
	for i, r := range reviews {
		rows[i] = []driver.Value{
			r.Org, r.Repo, r.PRNumber, r.ReviewID, r.ReviewerLogin,
			nullIfEmpty(r.State), r.CommitID, r.SubmittedAt, r.Href,
		}
	}

	return d.bulkUpsert(ctx, "reviews", reviewColumns, []string{"org", "repo", "pr_number", "review_id"}, rows)
}

var checkRunColumns = []string{
	"org", "repo", "commit_sha", "check_run_id", "check_name",
	"status", "conclusion", "completed_at",
}

// UpsertCheckRuns batch-inserts check runs using the DuckDB Appender API with
// a staging table for upsert semantics.
func (d *DB) UpsertCheckRuns(ctx context.Context, checkRuns []model.CheckRun) error {
	if len(checkRuns) == 0 {
		return nil
	}

	rows := make([][]driver.Value, len(checkRuns))
	for i, cr := range checkRuns {
		rows[i] = []driver.Value{
			cr.Org, cr.Repo, cr.CommitSHA, cr.CheckRunID, cr.CheckName,
			nullIfEmpty(cr.Status), nullIfEmpty(cr.Conclusion), cr.CompletedAt,
		}
	}

	return d.bulkUpsert(ctx, "check_runs", checkRunColumns, []string{"org", "repo", "commit_sha", "check_run_id"}, rows)
}

var commitPRColumns = []string{"org", "repo", "sha", "pr_number"}

// UpsertCommitPRs links a commit to its associated PR numbers.
func (d *DB) UpsertCommitPRs(ctx context.Context, org, repo, sha string, prNumbers []int) error {
	if len(prNumbers) == 0 {
		return nil
	}

	rows := make([][]driver.Value, len(prNumbers))
	for i, n := range prNumbers {
		rows[i] = []driver.Value{org, repo, sha, n}
	}

	return d.bulkUpsert(ctx, "commit_prs", commitPRColumns, []string{"org", "repo", "sha", "pr_number"}, rows)
}

// GetPRsForCommit retrieves pull requests associated with a commit via commit_prs.
func (d *DB) GetPRsForCommit(ctx context.Context, org, repo, sha string) ([]model.PullRequest, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT p.org, p.repo, p.number, p.title, p.merged, p.head_sha,
		       COALESCE(p.head_branch, ''), p.merge_commit_sha, p.author_login,
		       COALESCE(p.merged_by_login, ''), p.merged_at, p.href
		FROM pull_requests p
		INNER JOIN commit_prs cp ON p.org = cp.org AND p.repo = cp.repo AND p.number = cp.pr_number
		WHERE cp.org = ? AND cp.repo = ? AND cp.sha = ?`, org, repo, sha)
	if err != nil {
		return nil, fmt.Errorf("query PRs for commit: %w", err)
	}
	defer rows.Close()

	var result []model.PullRequest
	for rows.Next() {
		var pr model.PullRequest
		if err := rows.Scan(&pr.Org, &pr.Repo, &pr.Number, &pr.Title, &pr.Merged,
			&pr.HeadSHA, &pr.HeadBranch, &pr.MergeCommitSHA, &pr.AuthorLogin,
			&pr.MergedByLogin, &pr.MergedAt, &pr.Href); err != nil {
			return nil, fmt.Errorf("scan PR: %w", err)
		}
		result = append(result, pr)
	}
	return result, rows.Err()
}

// GetCommitsForPR retrieves commits associated with a PR via commit_prs.
func (d *DB) GetCommitsForPR(ctx context.Context, org, repo string, prNumber int) ([]model.Commit, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT c.org, c.repo, c.sha, c.author_login, c.author_email, c.committer_login,
		       c.committed_at, c.message, c.parent_count, c.additions, c.deletions, c.href
		FROM commits c
		INNER JOIN commit_prs cp ON c.org = cp.org AND c.repo = cp.repo AND c.sha = cp.sha
		WHERE cp.org = ? AND cp.repo = ? AND cp.pr_number = ?`, org, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("query commits for PR: %w", err)
	}
	defer rows.Close()

	commits, err := scanCommits(rows)
	if err != nil {
		return nil, err
	}
	if err := d.loadCoAuthorsForCommits(ctx, commits); err != nil {
		return nil, err
	}
	return commits, nil
}

// GetReviewsForPR retrieves reviews for a specific pull request.
func (d *DB) GetReviewsForPR(ctx context.Context, org, repo string, prNumber int) ([]model.Review, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT org, repo, pr_number, review_id, reviewer_login, COALESCE(state::TEXT, ''), commit_id, submitted_at, href
		FROM reviews
		WHERE org = ? AND repo = ? AND pr_number = ?
		ORDER BY submitted_at`, org, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("query reviews: %w", err)
	}
	defer rows.Close()

	var result []model.Review
	for rows.Next() {
		var r model.Review
		if err := rows.Scan(&r.Org, &r.Repo, &r.PRNumber, &r.ReviewID, &r.ReviewerLogin,
			&r.State, &r.CommitID, &r.SubmittedAt, &r.Href); err != nil {
			return nil, fmt.Errorf("scan review: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetCheckRunsForCommit retrieves check runs for a specific commit.
func (d *DB) GetCheckRunsForCommit(ctx context.Context, org, repo, sha string) ([]model.CheckRun, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT org, repo, commit_sha, check_run_id, check_name, COALESCE(status::TEXT, ''), COALESCE(conclusion::TEXT, ''), completed_at
		FROM check_runs
		WHERE org = ? AND repo = ? AND commit_sha = ?
		ORDER BY check_name`, org, repo, sha)
	if err != nil {
		return nil, fmt.Errorf("query check runs: %w", err)
	}
	defer rows.Close()

	var result []model.CheckRun
	for rows.Next() {
		var cr model.CheckRun
		if err := rows.Scan(&cr.Org, &cr.Repo, &cr.CommitSHA, &cr.CheckRunID, &cr.CheckName,
			&cr.Status, &cr.Conclusion, &cr.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan check run: %w", err)
		}
		result = append(result, cr)
	}
	return result, rows.Err()
}
