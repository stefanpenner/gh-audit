package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/model"
)

const batchSize = 500

// UpsertCommits batch-inserts commits using multi-value INSERT OR REPLACE.
func (d *DB) UpsertCommits(ctx context.Context, commits []model.Commit) error {
	if len(commits) == 0 {
		return nil
	}

	for i := 0; i < len(commits); i += batchSize {
		end := i + batchSize
		if end > len(commits) {
			end = len(commits)
		}
		if err := d.upsertCommitBatch(ctx, commits[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) upsertCommitBatch(ctx context.Context, commits []model.Commit) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	placeholders := make([]string, len(commits))
	args := make([]interface{}, 0, len(commits)*11)
	for i, c := range commits {
		placeholders[i] = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args, c.Org, c.Repo, c.SHA, c.AuthorLogin, c.AuthorEmail,
			c.CommittedAt, c.Message, c.ParentCount, c.Additions, c.Deletions, c.Href)
	}

	q := fmt.Sprintf(`INSERT OR REPLACE INTO commits
		(org, repo, sha, author_login, author_email, committed_at, message, parent_count, additions, deletions, href)
		VALUES %s`, strings.Join(placeholders, ", "))

	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("upsert commits: %w", err)
	}
	return tx.Commit()
}

// UpsertCommitBranches records which branch(es) a set of commits belong to.
func (d *DB) UpsertCommitBranches(ctx context.Context, org, repo string, shas []string, branch string) error {
	if len(shas) == 0 {
		return nil
	}

	for i := 0; i < len(shas); i += batchSize {
		end := i + batchSize
		if end > len(shas) {
			end = len(shas)
		}
		batch := shas[i:end]

		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)*4)
		for j, sha := range batch {
			placeholders[j] = "(?, ?, ?, ?)"
			args = append(args, org, repo, sha, branch)
		}

		q := fmt.Sprintf(`INSERT OR REPLACE INTO commit_branches (org, repo, sha, branch) VALUES %s`,
			strings.Join(placeholders, ", "))

		if _, err := d.DB.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("upsert commit branches: %w", err)
		}
	}
	return nil
}

// GetUnauditedCommits returns commits in org/repo that have no corresponding audit_results row.
func (d *DB) GetUnauditedCommits(ctx context.Context, org, repo string) ([]model.Commit, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT c.org, c.repo, c.sha, c.author_login, c.author_email,
		       c.committed_at, c.message, c.parent_count, c.additions, c.deletions, c.href
		FROM commits c
		LEFT JOIN audit_results a ON c.org = a.org AND c.repo = a.repo AND c.sha = a.sha
		WHERE c.org = ? AND c.repo = ? AND a.sha IS NULL
		ORDER BY c.committed_at`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query unaudited commits: %w", err)
	}
	defer rows.Close()

	return scanCommits(rows)
}

// GetCommitsBySHA retrieves specific commits by their SHAs.
func (d *DB) GetCommitsBySHA(ctx context.Context, org, repo string, shas []string) ([]model.Commit, error) {
	if len(shas) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(shas))
	args := make([]interface{}, 0, len(shas)+2)
	args = append(args, org, repo)
	for i, sha := range shas {
		placeholders[i] = "?"
		args = append(args, sha)
	}

	q := fmt.Sprintf(`SELECT org, repo, sha, author_login, author_email,
		committed_at, message, parent_count, additions, deletions, href
		FROM commits
		WHERE org = ? AND repo = ? AND sha IN (%s)`, strings.Join(placeholders, ", "))

	rows, err := d.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query commits by sha: %w", err)
	}
	defer rows.Close()

	return scanCommits(rows)
}

func scanCommits(rows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
}) ([]model.Commit, error) {
	var result []model.Commit
	for rows.Next() {
		var c model.Commit
		if err := rows.Scan(&c.Org, &c.Repo, &c.SHA, &c.AuthorLogin, &c.AuthorEmail,
			&c.CommittedAt, &c.Message, &c.ParentCount, &c.Additions, &c.Deletions, &c.Href); err != nil {
			return nil, fmt.Errorf("scan commit: %w", err)
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
