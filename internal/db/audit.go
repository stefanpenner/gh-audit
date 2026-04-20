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

// AuditQueryOpts controls filtering for GetAuditResults.
type AuditQueryOpts struct {
	Org          string
	Repo         string
	Since        time.Time
	Until        time.Time
	OnlyFailures bool
}

// AuditRow extends AuditResult with commit metadata for reporting.
type AuditRow struct {
	model.AuditResult
	AuthorLogin string
	CommittedAt time.Time
	Message     string
}

var auditResultColumns = []string{
	"org", "repo", "sha", "is_empty_commit", "is_bot", "is_exempt_author",
	"has_pr", "pr_number", "pr_count", "has_final_approval", "has_stale_approval",
	"has_post_merge_concern",
	"is_clean_revert", "revert_verification", "reverted_sha",
	"is_clean_merge", "merge_verification",
	"is_self_approved", "approver_logins", "owner_approval_check", "is_compliant",
	"reasons", "merge_strategy", "pr_commit_author_logins", "commit_href", "pr_href",
	"annotations",
}

// UpsertAuditResults batch-inserts audit results using the DuckDB Appender API
// with a staging table for upsert semantics.
func (d *DB) UpsertAuditResults(ctx context.Context, results []model.AuditResult) error {
	if len(results) == 0 {
		return nil
	}

	rows := make([][]driver.Value, len(results))
	for i, r := range results {
		rows[i] = []driver.Value{
			r.Org, r.Repo, r.SHA,
			r.IsEmptyCommit, r.IsBot, r.IsExemptAuthor, r.HasPR, r.PRNumber, r.PRCount,
			r.HasFinalApproval, r.HasStaleApproval,
			r.HasPostMergeConcern,
			r.IsCleanRevert, nullIfEmpty(r.RevertVerification), nullIfEmpty(r.RevertedSHA),
			r.IsCleanMerge, nullIfEmpty(r.MergeVerification),
			r.IsSelfApproved,
			toAnySlice(r.ApproverLogins),
			nullIfEmpty(r.OwnerApprovalCheck), r.IsCompliant,
			toAnySlice(r.Reasons),
			nullIfEmpty(r.MergeStrategy),
			toAnySlice(r.PRCommitAuthorLogins),
			r.CommitHref, r.PRHref,
			toAnySlice(r.Annotations),
		}
	}

	return d.bulkUpsert(ctx, "audit_results", auditResultColumns, []string{"org", "repo", "sha"}, rows)
}

// toAnySlice converts a []string to []any for the DuckDB Appender LIST type.
func toAnySlice(ss []string) []any {
	if ss == nil {
		return []any{}
	}
	result := make([]any, len(ss))
	for i, s := range ss {
		result[i] = s
	}
	return result
}

// nullIfEmpty returns nil if the string is empty, otherwise returns the string.
// Used to insert NULL for optional enum columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// scanDuckDBTextArray converts the value returned by DuckDB for a TEXT[] column
// into a Go []string. DuckDB's Go driver returns TEXT[] as []any.
func scanDuckDBTextArray(v any) []string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case []any:
		result := make([]string, 0, len(val))
		for _, elem := range val {
			if s, ok := elem.(string); ok {
				result = append(result, s)
			} else if elem != nil {
				result = append(result, fmt.Sprintf("%v", elem))
			}
		}
		if len(result) == 0 {
			return nil
		}
		return result
	case string:
		// Fallback: parse string representation
		s := strings.TrimSpace(val)
		if s == "" || s == "[]" {
			return nil
		}
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		parts := strings.Split(s, ", ")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				result = append(result, p)
			}
		}
		return result
	default:
		return nil
	}
}

// DeleteAuditResults removes all audit results for an org/repo so they can be re-inserted.
func (d *DB) DeleteAuditResults(ctx context.Context, org, repo string) error {
	_, err := d.DB.ExecContext(ctx, "DELETE FROM audit_results WHERE org = ? AND repo = ?", org, repo)
	if err != nil {
		return fmt.Errorf("delete audit results for %s/%s: %w", org, repo, err)
	}
	return nil
}

// RevertMergeClassification captures the enrichment-phase fields that the
// re-audit path cannot recompute (they require GetCommitFiles calls against
// the reverted commit, which re-audit intentionally skips). Callers that
// re-audit should read these from the existing audit_results row before
// deleting, then copy them onto the new AuditResult so the classification
// isn't clobbered.
type RevertMergeClassification struct {
	IsCleanRevert       bool
	RevertVerification  string
	RevertedSHA         string
	IsCleanMerge        bool
	MergeVerification   string
}

// GetRevertMergeClassification returns the stored revert/merge classification
// for a commit, or zero values if no audit row exists.
func (d *DB) GetRevertMergeClassification(ctx context.Context, org, repo, sha string) (RevertMergeClassification, error) {
	var c RevertMergeClassification
	row := d.DB.QueryRowContext(ctx, `
SELECT
  COALESCE(is_clean_revert, false),
  COALESCE(revert_verification, ''),
  COALESCE(reverted_sha, ''),
  COALESCE(is_clean_merge, false),
  COALESCE(merge_verification, '')
FROM audit_results
WHERE org = ? AND repo = ? AND sha = ?`, org, repo, sha)
	err := row.Scan(&c.IsCleanRevert, &c.RevertVerification, &c.RevertedSHA, &c.IsCleanMerge, &c.MergeVerification)
	if err == sql.ErrNoRows {
		return c, nil
	}
	return c, err
}

// DeleteAuditResultsBySHA removes a single audit result for (org, repo, sha).
// Used by the backfill path which re-audits a single commit after discovering
// its missing PR; DuckDB's INSERT OR REPLACE hits a "List Update is not
// supported" error when the existing row has non-empty LIST columns (reasons,
// approver_logins), so we delete-then-insert instead of relying on UPSERT.
func (d *DB) DeleteAuditResultsBySHA(ctx context.Context, org, repo, sha string) error {
	_, err := d.DB.ExecContext(ctx,
		"DELETE FROM audit_results WHERE org = ? AND repo = ? AND sha = ?",
		org, repo, sha)
	if err != nil {
		return fmt.Errorf("delete audit result for %s/%s@%s: %w", org, repo, sha[:min(12, len(sha))], err)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// DeleteOrphanedAuditResults removes audit results whose SHA no longer appears
// in the commits table. Safe to call after UpsertAuditResults — if it fails,
// the only consequence is stale rows (no data loss).
func (d *DB) DeleteOrphanedAuditResults(ctx context.Context, org, repo string) error {
	_, err := d.DB.ExecContext(ctx, `
		DELETE FROM audit_results
		WHERE org = ? AND repo = ?
		  AND sha NOT IN (SELECT sha FROM commits WHERE org = ? AND repo = ?)`,
		org, repo, org, repo)
	if err != nil {
		return fmt.Errorf("delete orphaned audit results for %s/%s: %w", org, repo, err)
	}
	return nil
}

// GetAuditResults retrieves audit results with optional filters, joined with commit data.
func (d *DB) GetAuditResults(ctx context.Context, opts AuditQueryOpts) ([]AuditRow, error) {
	var conditions []string
	var args []any

	if opts.Org != "" {
		conditions = append(conditions, "a.org = ?")
		args = append(args, opts.Org)
	}
	if opts.Repo != "" {
		conditions = append(conditions, "a.repo = ?")
		args = append(args, opts.Repo)
	}
	if !opts.Since.IsZero() {
		conditions = append(conditions, "c.committed_at >= ?")
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		conditions = append(conditions, "c.committed_at < ?")
		args = append(args, opts.Until)
	}
	if opts.OnlyFailures {
		conditions = append(conditions, "a.is_compliant = false")
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	q := fmt.Sprintf(`
		SELECT a.org, a.repo, a.sha, a.is_empty_commit, a.is_bot, a.is_exempt_author, a.has_pr, a.pr_number,
		       COALESCE(a.pr_count, 0), a.has_final_approval, COALESCE(a.has_stale_approval, false),
		       COALESCE(a.has_post_merge_concern, false),
		       COALESCE(a.is_clean_revert, false), COALESCE(a.revert_verification, ''), COALESCE(a.reverted_sha, ''),
		       COALESCE(a.is_clean_merge, false), COALESCE(a.merge_verification, ''),
		       a.is_self_approved, a.approver_logins, COALESCE(a.owner_approval_check::TEXT, ''), a.is_compliant,
		       a.reasons, COALESCE(a.merge_strategy, ''), a.commit_href, a.pr_href, a.audited_at,
		       COALESCE(a.annotations, []::TEXT[]),
		       COALESCE(c.author_login, ''), COALESCE(c.committed_at, '1970-01-01'::TIMESTAMP), COALESCE(c.message, '')
		FROM audit_results a
		LEFT JOIN commits c ON a.org = c.org AND a.repo = c.repo AND a.sha = c.sha
		%s
		ORDER BY c.committed_at DESC`, where)

	rows, err := d.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit results: %w", err)
	}
	defer rows.Close()

	var result []AuditRow
	for rows.Next() {
		var row AuditRow
		var approvers, reasons, annotations any
		if err := rows.Scan(
			&row.Org, &row.Repo, &row.SHA,
			&row.IsEmptyCommit, &row.IsBot, &row.IsExemptAuthor, &row.HasPR, &row.PRNumber,
			&row.PRCount, &row.HasFinalApproval, &row.HasStaleApproval,
			&row.HasPostMergeConcern,
			&row.IsCleanRevert, &row.RevertVerification, &row.RevertedSHA,
			&row.IsCleanMerge, &row.MergeVerification,
			&row.IsSelfApproved, &approvers, &row.OwnerApprovalCheck,
			&row.IsCompliant, &reasons, &row.MergeStrategy, &row.CommitHref, &row.PRHref, &row.AuditedAt,
			&annotations,
			&row.AuthorLogin, &row.CommittedAt, &row.Message,
		); err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		row.ApproverLogins = scanDuckDBTextArray(approvers)
		row.Reasons = scanDuckDBTextArray(reasons)
		row.Annotations = scanDuckDBTextArray(annotations)
		result = append(result, row)
	}
	return result, rows.Err()
}
