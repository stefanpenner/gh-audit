package db

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/model"
)

const batchSize = 500

var commitColumns = []string{
	"org", "repo", "sha", "author_login", "author_email", "committer_login",
	"committed_at", "message", "parent_count", "additions", "deletions", "href",
}

// UpsertCommits batch-inserts commits using the DuckDB Appender API with
// a staging table for upsert semantics.
func (d *DB) UpsertCommits(ctx context.Context, commits []model.Commit) error {
	if len(commits) == 0 {
		return nil
	}

	rows := make([][]driver.Value, len(commits))
	for i, c := range commits {
		rows[i] = []driver.Value{
			c.Org, c.Repo, c.SHA, c.AuthorLogin, c.AuthorEmail, c.CommitterLogin,
			c.CommittedAt, c.Message, c.ParentCount, c.Additions, c.Deletions, c.Href,
		}
	}

	return d.bulkUpsert(ctx, "commits", commitColumns, rows)
}

var commitBranchColumns = []string{"org", "repo", "sha", "branch"}

// UpsertCommitBranches records which branch(es) a set of commits belong to.
func (d *DB) UpsertCommitBranches(ctx context.Context, org, repo string, shas []string, branch string) error {
	if len(shas) == 0 {
		return nil
	}

	rows := make([][]driver.Value, len(shas))
	for i, sha := range shas {
		rows[i] = []driver.Value{org, repo, sha, branch}
	}

	return d.bulkUpsert(ctx, "commit_branches", commitBranchColumns, rows)
}

// GetUnauditedCommits returns commits in org/repo that have no corresponding audit_results row.
func (d *DB) GetUnauditedCommits(ctx context.Context, org, repo string) ([]model.Commit, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT c.org, c.repo, c.sha, c.author_login, c.author_email, c.committer_login,
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

// GetAllCommits returns all commits for an org/repo.
func (d *DB) GetAllCommits(ctx context.Context, org, repo string) ([]model.Commit, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT org, repo, sha, author_login, author_email, committer_login,
		       committed_at, message, parent_count, additions, deletions, href
		FROM commits
		WHERE org = ? AND repo = ?
		ORDER BY committed_at`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query all commits: %w", err)
	}
	defer rows.Close()

	return scanCommits(rows)
}

// UpdateCommitStats updates the additions and deletions for a commit.
func (d *DB) UpdateCommitStats(ctx context.Context, org, repo, sha string, additions, deletions int) error {
	_, err := d.DB.ExecContext(ctx,
		"UPDATE commits SET additions = ?, deletions = ? WHERE org = ? AND repo = ? AND sha = ?",
		additions, deletions, org, repo, sha)
	if err != nil {
		return fmt.Errorf("update commit stats %s/%s@%s: %w", org, repo, sha[:12], err)
	}
	return nil
}

// GetCommitsBySHA retrieves specific commits by their SHAs.
func (d *DB) GetCommitsBySHA(ctx context.Context, org, repo string, shas []string) ([]model.Commit, error) {
	if len(shas) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(shas))
	args := make([]any, 0, len(shas)+2)
	args = append(args, org, repo)
	for i, sha := range shas {
		placeholders[i] = "?"
		args = append(args, sha)
	}

	q := fmt.Sprintf(`SELECT org, repo, sha, author_login, author_email, committer_login,
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
	Scan(...any) error
	Err() error
}) ([]model.Commit, error) {
	var result []model.Commit
	for rows.Next() {
		var c model.Commit
		if err := rows.Scan(&c.Org, &c.Repo, &c.SHA, &c.AuthorLogin, &c.AuthorEmail, &c.CommitterLogin,
			&c.CommittedAt, &c.Message, &c.ParentCount, &c.Additions, &c.Deletions, &c.Href); err != nil {
			return nil, fmt.Errorf("scan commit: %w", err)
		}
		c.CoAuthors = model.ParseCoAuthors(c.Message)
		result = append(result, c)
	}
	return result, rows.Err()
}
