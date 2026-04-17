package db

import (
	"context"
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

// UpsertAuditResults batch-inserts audit results using multi-value INSERT OR REPLACE.
func (d *DB) UpsertAuditResults(ctx context.Context, results []model.AuditResult) error {
	if len(results) == 0 {
		return nil
	}
	for i := 0; i < len(results); i += batchSize {
		end := i + batchSize
		if end > len(results) {
			end = len(results)
		}
		if err := d.upsertAuditBatch(ctx, results[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) upsertAuditBatch(ctx context.Context, results []model.AuditResult) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	placeholders := make([]string, len(results))
	args := make([]interface{}, 0, len(results)*14)
	for i, r := range results {
		placeholders[i] = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args,
			r.Org, r.Repo, r.SHA,
			r.IsEmptyCommit, r.IsBot, r.HasPR, r.PRNumber,
			r.HasFinalApproval,
			toDuckDBList(r.ApproverLogins),
			r.OwnerApprovalCheck, r.IsCompliant,
			toDuckDBList(r.Reasons),
			r.CommitHref, r.PRHref,
		)
	}

	q := fmt.Sprintf(`INSERT OR REPLACE INTO audit_results
		(org, repo, sha, is_empty_commit, is_bot, has_pr, pr_number,
		 has_final_approval, approver_logins, owner_approval_check, is_compliant,
		 reasons, commit_href, pr_href)
		VALUES %s`, strings.Join(placeholders, ", "))

	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("upsert audit results: %w", err)
	}
	return tx.Commit()
}

// toDuckDBList converts a Go string slice to a DuckDB list literal like ['a','b'].
func toDuckDBList(ss []string) string {
	if len(ss) == 0 {
		return "[]"
	}
	escaped := make([]string, len(ss))
	for i, s := range ss {
		escaped[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "[" + strings.Join(escaped, ", ") + "]"
}

// scanDuckDBTextArray converts the value returned by DuckDB for a TEXT[] column
// into a Go []string. DuckDB's Go driver returns TEXT[] as []interface{}.
func scanDuckDBTextArray(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case []interface{}:
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

// GetAuditResults retrieves audit results with optional filters, joined with commit data.
func (d *DB) GetAuditResults(ctx context.Context, opts AuditQueryOpts) ([]AuditRow, error) {
	var conditions []string
	var args []interface{}

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
		SELECT a.org, a.repo, a.sha, a.is_empty_commit, a.is_bot, a.has_pr, a.pr_number,
		       a.has_final_approval, a.approver_logins, a.owner_approval_check, a.is_compliant,
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
		var approvers, reasons interface{}
		if err := rows.Scan(
			&row.Org, &row.Repo, &row.SHA,
			&row.IsEmptyCommit, &row.IsBot, &row.HasPR, &row.PRNumber,
			&row.HasFinalApproval, &approvers, &row.OwnerApprovalCheck,
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
