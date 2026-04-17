package db

import (
	"context"
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
	"is_self_approved", "approver_logins", "owner_approval_check", "is_compliant",
	"reasons", "merge_strategy", "commit_href", "pr_href",
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
			r.HasFinalApproval, r.HasStaleApproval, r.IsSelfApproved,
			toAnySlice(r.ApproverLogins),
			nullIfEmpty(r.OwnerApprovalCheck), r.IsCompliant,
			toAnySlice(r.Reasons),
			nullIfEmpty(r.MergeStrategy),
			r.CommitHref, r.PRHref,
		}
	}

	return d.bulkUpsert(ctx, "audit_results", auditResultColumns, rows)
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
		       a.has_final_approval, a.is_self_approved, a.approver_logins, COALESCE(a.owner_approval_check::TEXT, ''), a.is_compliant,
		       a.reasons, a.commit_href, a.pr_href, a.audited_at,
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
		var approvers, reasons any
		if err := rows.Scan(
			&row.Org, &row.Repo, &row.SHA,
			&row.IsEmptyCommit, &row.IsBot, &row.IsExemptAuthor, &row.HasPR, &row.PRNumber,
			&row.HasFinalApproval, &row.IsSelfApproved, &approvers, &row.OwnerApprovalCheck,
			&row.IsCompliant, &reasons, &row.CommitHref, &row.PRHref, &row.AuditedAt,
			&row.AuthorLogin, &row.CommittedAt, &row.Message,
		); err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		row.ApproverLogins = scanDuckDBTextArray(approvers)
		row.Reasons = scanDuckDBTextArray(reasons)
		result = append(result, row)
	}
	return result, rows.Err()
}
