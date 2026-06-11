package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

var commitColumns = []string{
	"org", "repo", "sha", "author_login", "author_id", "author_email", "committer_login",
	"committed_at", "message", "parent_count", "additions", "deletions",
	"files_changed", "detail_fetched_at", "is_verified", "href",
}

// commitRow maps a model.Commit to the commitColumns order. files_changed
// and detail_fetched_at are NULL unless the commit's stats were verified by
// a real GET /commits/{sha} fetch — NULL is what lets the preservation
// UPDATE distinguish "never fetched" from "verified zero".
func commitRow(c model.Commit) []driver.Value {
	var filesChanged, detailFetchedAt driver.Value
	if c.StatsVerified {
		filesChanged = c.FilesChanged
		detailFetchedAt = time.Now().UTC()
	}
	return []driver.Value{
		c.Org, c.Repo, c.SHA, c.AuthorLogin, c.AuthorID, c.AuthorEmail, c.CommitterLogin,
		c.CommittedAt, c.Message, c.ParentCount, c.Additions, c.Deletions,
		filesChanged, detailFetchedAt, c.IsVerified, c.Href,
	}
}

// commitSelectColumns is the canonical SELECT list matching scanCommits'
// scan order. detail_fetched_at surfaces as the StatsVerified boolean.
const commitSelectColumns = `org, repo, sha, author_login, author_id, author_email, committer_login,
	committed_at, message, parent_count, additions, deletions, COALESCE(files_changed, 0),
	detail_fetched_at IS NOT NULL, is_verified, href`

// preserveCommitDetailSQL guards lazily-fetched commit detail from being
// clobbered by stat-less re-ingestion. List/compare endpoints never carry
// diff stats, and the 72h cursor overlap re-lists already-synced commits
// on every date-window sync — a full-row REPLACE would zero the
// additions/deletions/files_changed persisted by MarkCommitDetail, and an
// offline re-audit would then read 0/0 as "empty" and mint a false
// compliant waiver. Runs against the staging table before the merge.
const preserveCommitDetailSQL = `
	UPDATE staging_commits s
	SET additions = c.additions,
	    deletions = c.deletions,
	    files_changed = c.files_changed,
	    detail_fetched_at = c.detail_fetched_at
	FROM commits c
	WHERE s.org = c.org AND s.repo = c.repo AND s.sha = c.sha
	  AND s.detail_fetched_at IS NULL
	  AND c.detail_fetched_at IS NOT NULL`

// UpsertCommits batch-inserts commits using the DuckDB Appender API with
// a staging table for upsert semantics.
func (d *DB) UpsertCommits(ctx context.Context, commits []model.Commit) error {
	if len(commits) == 0 {
		return nil
	}

	rows := make([][]driver.Value, len(commits))
	for i, c := range commits {
		rows[i] = commitRow(c)
	}

	return d.bulkUpsert(ctx, "commits", commitColumns, []string{"org", "repo", "sha"}, rows, preserveCommitDetailSQL)
}

// InsertCommitsIfAbsent inserts commits whose (org, repo, sha) is not yet
// present and leaves existing rows untouched. For PR-branch commits from
// /pulls/{n}/commits, whose rows lack author_email, href, is_verified, and
// diff stats: a blind upsert would replace rows ingested rich by phase 1's
// ListCommits (or enriched by the lazy stats fetcher) with gutted copies,
// breaking the §1 verified_emails fallback and merge classification on
// every later DB read.
func (d *DB) InsertCommitsIfAbsent(ctx context.Context, commits []model.Commit) error {
	if len(commits) == 0 {
		return nil
	}
	rows := make([][]driver.Value, len(commits))
	for i, c := range commits {
		rows[i] = commitRow(c)
	}
	return d.bulkInsertIfAbsent(ctx, "commits", commitColumns, []string{"org", "repo", "sha"}, rows)
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

	return d.bulkUpsert(ctx, "commit_branches", commitBranchColumns, []string{"org", "repo", "sha", "branch"}, rows)
}

// GetUnauditedCommits returns commits in org/repo that have no corresponding
// audit_results row. since/until bound the result by committed_at; either may
// be zero to disable that side of the bound. Both zero = unbounded (mops up
// the full historical backlog, matching the original behaviour).
func (d *DB) GetUnauditedCommits(ctx context.Context, org, repo string, since, until time.Time) ([]model.Commit, error) {
	q := `
		SELECT c.org, c.repo, c.sha, c.author_login, c.author_id, c.author_email, c.committer_login,
		       c.committed_at, c.message, c.parent_count, c.additions, c.deletions,
		       COALESCE(c.files_changed, 0), c.detail_fetched_at IS NOT NULL, c.is_verified, c.href
		FROM commits c
		LEFT JOIN audit_results a ON c.org = a.org AND c.repo = a.repo AND c.sha = a.sha
		WHERE c.org = ? AND c.repo = ? AND a.sha IS NULL`
	args := []any{org, repo}
	if !since.IsZero() {
		q += " AND c.committed_at >= ?"
		args = append(args, since)
	}
	if !until.IsZero() {
		q += " AND c.committed_at < ?"
		args = append(args, until)
	}
	q += " ORDER BY c.committed_at"

	rows, err := d.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query unaudited commits: %w", err)
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

// GetAllCommits returns all commits for an org/repo.
func (d *DB) GetAllCommits(ctx context.Context, org, repo string) ([]model.Commit, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT `+commitSelectColumns+`
		FROM commits
		WHERE org = ? AND repo = ?
		ORDER BY committed_at`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query all commits: %w", err)
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

// UpdateCommitStats updates the additions and deletions for a commit.
func (d *DB) UpdateCommitStats(ctx context.Context, org, repo, sha string, additions, deletions int) error {
	_, err := d.DB.ExecContext(ctx,
		"UPDATE commits SET additions = ?, deletions = ? WHERE org = ? AND repo = ? AND sha = ?",
		additions, deletions, org, repo, sha)
	if err != nil {
		return fmt.Errorf("update commit stats %s/%s@%s: %w", org, repo, sha[:min(12, len(sha))], err)
	}
	return nil
}

// MarkCommitDetail persists the authoritative stats from a real
// GET /commits/{sha} fetch and stamps detail_fetched_at, making "verified
// zero" (a truly empty commit) distinguishable from "never fetched" on
// every later read — sync-time verdicts and offline re-audits then agree.
func (d *DB) MarkCommitDetail(ctx context.Context, org, repo, sha string, additions, deletions, filesChanged int) error {
	_, err := d.DB.ExecContext(ctx,
		`UPDATE commits
		 SET additions = ?, deletions = ?, files_changed = ?, detail_fetched_at = current_timestamp
		 WHERE org = ? AND repo = ? AND sha = ?`,
		additions, deletions, filesChanged, org, repo, sha)
	if err != nil {
		return fmt.Errorf("mark commit detail %s/%s@%s: %w", org, repo, sha[:min(12, len(sha))], err)
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

	q := fmt.Sprintf(`SELECT `+commitSelectColumns+`
		FROM commits
		WHERE org = ? AND repo = ? AND sha IN (%s)`, strings.Join(placeholders, ", "))

	rows, err := d.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query commits by sha: %w", err)
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

var coAuthorColumns = []string{"org", "repo", "sha", "name", "email", "login"}

// UpsertCoAuthors batch-inserts co-authors for a set of commits.
func (d *DB) UpsertCoAuthors(ctx context.Context, commits []model.Commit) error {
	var rows [][]driver.Value
	for _, c := range commits {
		for _, ca := range c.CoAuthors {
			rows = append(rows, []driver.Value{
				c.Org, c.Repo, c.SHA,
				ca.Name, ca.Email, nullIfEmptyStr(ca.Login),
			})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return d.bulkUpsert(ctx, "co_authors", coAuthorColumns, []string{"org", "repo", "sha", "email"}, rows)
}

func nullIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetCoAuthors returns co-authors for a single commit.
func (d *DB) GetCoAuthors(ctx context.Context, org, repo, sha string) ([]model.CoAuthor, error) {
	rows, err := d.DB.QueryContext(ctx,
		`SELECT COALESCE(name, ''), email, COALESCE(login, '') FROM co_authors WHERE org = ? AND repo = ? AND sha = ? ORDER BY COALESCE(name, ''), email`,
		org, repo, sha)
	if err != nil {
		return nil, fmt.Errorf("query co-authors: %w", err)
	}
	defer rows.Close()

	var result []model.CoAuthor
	for rows.Next() {
		var ca model.CoAuthor
		if err := rows.Scan(&ca.Name, &ca.Email, &ca.Login); err != nil {
			return nil, fmt.Errorf("scan co-author: %w", err)
		}
		result = append(result, ca)
	}
	return result, rows.Err()
}

// coAuthorSHAScopeLimit bounds when loadCoAuthorsForCommits scopes its
// query to the requested SHAs. Small lookups (the per-commit enrichment
// path calls GetCommitsBySHA for ONE sha) used to scan the whole repo's
// co_authors per call — O(commits x repo_coauthors) across a sweep. Big
// batches keep the single whole-repo scan, which is cheaper than a
// thousands-long IN list.
const coAuthorSHAScopeLimit = 200

// loadCoAuthorsForCommits bulk-loads co-authors for a set of commits and attaches them.
func (d *DB) loadCoAuthorsForCommits(ctx context.Context, commits []model.Commit) error {
	if len(commits) == 0 {
		return nil
	}

	org := commits[0].Org
	repo := commits[0].Repo

	q := `SELECT sha, COALESCE(name, ''), email, COALESCE(login, '') FROM co_authors WHERE org = ? AND repo = ?`
	args := []any{org, repo}
	if len(commits) <= coAuthorSHAScopeLimit {
		placeholders := make([]string, len(commits))
		for i, c := range commits {
			placeholders[i] = "?"
			args = append(args, c.SHA)
		}
		q += " AND sha IN (" + strings.Join(placeholders, ", ") + ")"
	}
	q += ` ORDER BY sha, COALESCE(name, ''), email`

	rows, err := d.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("query co-authors for %s/%s: %w", org, repo, err)
	}
	defer rows.Close()

	bySHA := make(map[string][]model.CoAuthor)
	for rows.Next() {
		var sha string
		var ca model.CoAuthor
		if err := rows.Scan(&sha, &ca.Name, &ca.Email, &ca.Login); err != nil {
			return fmt.Errorf("scan co-author: %w", err)
		}
		bySHA[sha] = append(bySHA[sha], ca)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range commits {
		if cas, ok := bySHA[commits[i].SHA]; ok {
			commits[i].CoAuthors = cas
		}
	}
	return nil
}

// scanCommits scans rows in the canonical commit column order. Every nullable
// column goes through a Null* wrapper: rows written before a column existed
// (e.g. committer_login, added by migration with no DEFAULT) carry NULL
// permanently, and a plain string/int scan would error on the first legacy
// row, bricking every read path on upgraded databases.
func scanCommits(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]model.Commit, error) {
	var result []model.Commit
	for rows.Next() {
		var c model.Commit
		var authorID sql.NullInt64
		var authorLogin, authorEmail, committerLogin, message, href sql.NullString
		var parentCount, additions, deletions, filesChanged sql.NullInt32
		if err := rows.Scan(&c.Org, &c.Repo, &c.SHA, &authorLogin, &authorID, &authorEmail, &committerLogin,
			&c.CommittedAt, &message, &parentCount, &additions, &deletions,
			&filesChanged, &c.StatsVerified, &c.IsVerified, &href); err != nil {
			return nil, fmt.Errorf("scan commit: %w", err)
		}
		c.AuthorLogin = authorLogin.String
		c.AuthorEmail = authorEmail.String
		c.CommitterLogin = committerLogin.String
		c.Message = message.String
		c.Href = href.String
		c.ParentCount = int(parentCount.Int32)
		c.Additions = int(additions.Int32)
		c.Deletions = int(deletions.Int32)
		c.FilesChanged = int(filesChanged.Int32)
		if authorID.Valid {
			c.AuthorID = authorID.Int64
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
