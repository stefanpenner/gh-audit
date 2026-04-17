package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// UpsertPullRequests batch-inserts pull requests using multi-value INSERT OR REPLACE.
func (d *DB) UpsertPullRequests(ctx context.Context, prs []model.PullRequest) error {
	if len(prs) == 0 {
		return nil
	}
	for i := 0; i < len(prs); i += batchSize {
		end := min(i+batchSize, len(prs))
		if err := d.upsertPRBatch(ctx, prs[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) upsertPRBatch(ctx context.Context, prs []model.PullRequest) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	placeholders := make([]string, len(prs))
	args := make([]any, 0, len(prs)*10)
	for i, pr := range prs {
		placeholders[i] = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args, pr.Org, pr.Repo, pr.Number, pr.Title, pr.Merged,
			pr.HeadSHA, pr.MergeCommitSHA, pr.AuthorLogin, pr.MergedAt, pr.Href)
	}

	q := fmt.Sprintf(`INSERT OR REPLACE INTO pull_requests
		(org, repo, number, title, merged, head_sha, merge_commit_sha, author_login, merged_at, href)
		VALUES %s`, strings.Join(placeholders, ", "))

	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("upsert pull requests: %w", err)
	}
	return tx.Commit()
}

// UpsertReviews batch-inserts reviews using multi-value INSERT OR REPLACE.
func (d *DB) UpsertReviews(ctx context.Context, reviews []model.Review) error {
	if len(reviews) == 0 {
		return nil
	}
	for i := 0; i < len(reviews); i += batchSize {
		end := min(i+batchSize, len(reviews))
		if err := d.upsertReviewBatch(ctx, reviews[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) upsertReviewBatch(ctx context.Context, reviews []model.Review) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	placeholders := make([]string, len(reviews))
	args := make([]any, 0, len(reviews)*9)
	for i, r := range reviews {
		placeholders[i] = "(?, ?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args, r.Org, r.Repo, r.PRNumber, r.ReviewID, r.ReviewerLogin,
			r.State, r.CommitID, r.SubmittedAt, r.Href)
	}

	q := fmt.Sprintf(`INSERT OR REPLACE INTO reviews
		(org, repo, pr_number, review_id, reviewer_login, state, commit_id, submitted_at, href)
		VALUES %s`, strings.Join(placeholders, ", "))

	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("upsert reviews: %w", err)
	}
	return tx.Commit()
}

// UpsertCheckRuns batch-inserts check runs using multi-value INSERT OR REPLACE.
func (d *DB) UpsertCheckRuns(ctx context.Context, checkRuns []model.CheckRun) error {
	if len(checkRuns) == 0 {
		return nil
	}
	for i := 0; i < len(checkRuns); i += batchSize {
		end := min(i+batchSize, len(checkRuns))
		if err := d.upsertCheckRunBatch(ctx, checkRuns[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) upsertCheckRunBatch(ctx context.Context, checkRuns []model.CheckRun) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	placeholders := make([]string, len(checkRuns))
	args := make([]any, 0, len(checkRuns)*8)
	for i, cr := range checkRuns {
		placeholders[i] = "(?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args, cr.Org, cr.Repo, cr.CommitSHA, cr.CheckRunID, cr.CheckName,
			cr.Status, cr.Conclusion, cr.CompletedAt)
	}

	q := fmt.Sprintf(`INSERT OR REPLACE INTO check_runs
		(org, repo, commit_sha, check_run_id, check_name, status, conclusion, completed_at)
		VALUES %s`, strings.Join(placeholders, ", "))

	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("upsert check runs: %w", err)
	}
	return tx.Commit()
}

// UpsertCommitPRs links a commit to its associated PR numbers.
func (d *DB) UpsertCommitPRs(ctx context.Context, org, repo, sha string, prNumbers []int) error {
	if len(prNumbers) == 0 {
		return nil
	}

	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	placeholders := make([]string, len(prNumbers))
	args := make([]any, 0, len(prNumbers)*4)
	for i, n := range prNumbers {
		placeholders[i] = "(?, ?, ?, ?)"
		args = append(args, org, repo, sha, n)
	}

	q := fmt.Sprintf(`INSERT OR REPLACE INTO commit_prs (org, repo, sha, pr_number) VALUES %s`,
		strings.Join(placeholders, ", "))

	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("upsert commit_prs: %w", err)
	}
	return tx.Commit()
}

// GetPRsForCommit retrieves pull requests associated with a commit via commit_prs.
func (d *DB) GetPRsForCommit(ctx context.Context, org, repo, sha string) ([]model.PullRequest, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT p.org, p.repo, p.number, p.title, p.merged, p.head_sha,
		       p.merge_commit_sha, p.author_login, p.merged_at, p.href
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
			&pr.HeadSHA, &pr.MergeCommitSHA, &pr.AuthorLogin, &pr.MergedAt, &pr.Href); err != nil {
			return nil, fmt.Errorf("scan PR: %w", err)
		}
		result = append(result, pr)
	}
	return result, rows.Err()
}

// GetReviewsForPR retrieves reviews for a specific pull request.
func (d *DB) GetReviewsForPR(ctx context.Context, org, repo string, prNumber int) ([]model.Review, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT org, repo, pr_number, review_id, reviewer_login, state, commit_id, submitted_at, href
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
		SELECT org, repo, commit_sha, check_run_id, check_name, status, conclusion, completed_at
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
