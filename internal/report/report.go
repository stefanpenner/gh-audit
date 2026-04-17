package report

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Reporter generates audit reports from the database.
type Reporter struct {
	db *sql.DB
}

// RepoFilter identifies a single org/repo pair for filtering.
type RepoFilter struct {
	Org  string
	Repo string
}

// ReportOpts controls report filtering.
type ReportOpts struct {
	Org          string       // filter by single org (used when --org is set without --repo)
	Repos        []RepoFilter // org/repo pairs from --repo flags; takes precedence over Org
	Since        time.Time
	Until        time.Time
	OnlyFailures bool
}

// SummaryRow is a per-repo compliance summary.
type SummaryRow struct {
	Org               string  `json:"org"`
	Repo              string  `json:"repo"`
	TotalCommits      int     `json:"total_commits"`
	CompliantCount    int     `json:"compliant_count"`
	NonCompliantCount int     `json:"non_compliant_count"`
	BotCount          int     `json:"bot_count"`
	ExemptCount       int     `json:"exempt_count"`
	EmptyCount        int     `json:"empty_count"`
	SelfApprovedCount int     `json:"self_approved_count"`
	CompliancePct     float64 `json:"compliance_pct"`
}

// DetailRow is a single commit's audit detail.
type DetailRow struct {
	Org                string    `json:"org"`
	Repo               string    `json:"repo"`
	SHA                string    `json:"sha"`
	AuthorLogin        string    `json:"author_login"`
	CommitterLogin     string    `json:"committer_login"`
	CommittedAt        time.Time `json:"committed_at"`
	Message            string    `json:"message"`
	IsBot              bool      `json:"is_bot"`
	IsExemptAuthor     bool      `json:"is_exempt_author"`
	IsEmptyCommit      bool      `json:"is_empty_commit"`
	IsSelfApproved     bool      `json:"is_self_approved"`
	HasPR              bool      `json:"has_pr"`
	PRNumber           int       `json:"pr_number"`
	PRHref             string    `json:"pr_href"`
	MergedByLogin      string    `json:"merged_by_login"`
	HasFinalApproval   bool      `json:"has_final_approval"`
	ApproverLogins     string    `json:"approver_logins"`
	OwnerApprovalCheck string    `json:"owner_approval_check"`
	IsCompliant        bool      `json:"is_compliant"`
	Reasons            string    `json:"reasons"`
	CommitHref         string    `json:"commit_href"`
	BranchName         string    `json:"branch_name"`
}

// New creates a new Reporter.
func New(db *sql.DB) *Reporter {
	return &Reporter{db: db}
}

// appendRepoFilter appends org/repo WHERE clauses to a query builder.
func appendRepoFilter(query string, args []any, opts ReportOpts) (string, []any) {
	if len(opts.Repos) > 0 {
		clauses := make([]string, len(opts.Repos))
		for i, rf := range opts.Repos {
			clauses[i] = "(a.org = ? AND a.repo = ?)"
			args = append(args, rf.Org, rf.Repo)
		}
		query += " AND (" + strings.Join(clauses, " OR ") + ")"
	} else if opts.Org != "" {
		query += " AND a.org = ?"
		args = append(args, opts.Org)
	}
	return query, args
}

// GetSummary returns per-repo compliance summary rows.
func (r *Reporter) GetSummary(ctx context.Context, opts ReportOpts) ([]SummaryRow, error) {
	query := `
		SELECT
			a.org,
			a.repo,
			COUNT(*) AS total_commits,
			COUNT(*) FILTER (WHERE a.is_compliant = true) AS compliant_count,
			COUNT(*) FILTER (WHERE a.is_compliant = false) AS non_compliant_count,
			COUNT(*) FILTER (WHERE a.is_bot = true) AS bot_count,
			COUNT(*) FILTER (WHERE a.is_exempt_author = true) AS exempt_count,
			COUNT(*) FILTER (WHERE a.is_empty_commit = true) AS empty_count,
			COUNT(*) FILTER (WHERE a.is_self_approved = true) AS self_approved_count
		FROM audit_results a
		JOIN commits c ON a.org = c.org AND a.repo = c.repo AND a.sha = c.sha
		WHERE 1=1
	`

	args := []any{}
	query, args = appendRepoFilter(query, args, opts)
	if !opts.Since.IsZero() {
		query += " AND c.committed_at >= ?"
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		query += " AND c.committed_at <= ?"
		args = append(args, opts.Until)
	}

	query += " GROUP BY a.org, a.repo ORDER BY a.org, a.repo"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query summary: %w", err)
	}
	defer rows.Close()

	var result []SummaryRow
	for rows.Next() {
		var s SummaryRow
		if err := rows.Scan(&s.Org, &s.Repo, &s.TotalCommits,
			&s.CompliantCount, &s.NonCompliantCount, &s.BotCount, &s.ExemptCount, &s.EmptyCount, &s.SelfApprovedCount); err != nil {
			return nil, fmt.Errorf("scan summary: %w", err)
		}
		if s.TotalCommits > 0 {
			s.CompliancePct = float64(s.CompliantCount) / float64(s.TotalCommits) * 100.0
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// GetDetails returns per-commit audit detail rows.
func (r *Reporter) GetDetails(ctx context.Context, opts ReportOpts) ([]DetailRow, error) {
	query := `
		SELECT
			a.org,
			a.repo,
			a.sha,
			COALESCE(c.author_login, ''),
			COALESCE(c.author_login, ''),
			COALESCE(c.committed_at, '1970-01-01'::TIMESTAMP),
			COALESCE(c.message, ''),
			a.is_bot,
			COALESCE(a.is_exempt_author, false),
			a.is_empty_commit,
			COALESCE(a.is_self_approved, false),
			a.has_pr,
			COALESCE(a.pr_number, 0),
			COALESCE(a.pr_href, ''),
			COALESCE(p.merged_by_login, ''),
			a.has_final_approval,
			COALESCE(array_to_string(a.approver_logins, ', '), ''),
			COALESCE(a.owner_approval_check::TEXT, ''),
			a.is_compliant,
			COALESCE(array_to_string(a.reasons, ', '), ''),
			COALESCE(a.commit_href, ''),
			COALESCE(cb.branch, '')
		FROM audit_results a
		JOIN commits c ON a.org = c.org AND a.repo = c.repo AND a.sha = c.sha
		LEFT JOIN pull_requests p ON a.org = p.org AND a.repo = p.repo AND a.pr_number = p.number
		LEFT JOIN commit_branches cb ON a.org = cb.org AND a.repo = cb.repo AND a.sha = cb.sha
		WHERE 1=1
	`

	args := []any{}
	query, args = appendRepoFilter(query, args, opts)
	if !opts.Since.IsZero() {
		query += " AND c.committed_at >= ?"
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		query += " AND c.committed_at <= ?"
		args = append(args, opts.Until)
	}
	if opts.OnlyFailures {
		query += " AND a.is_compliant = false"
	}

	query += " ORDER BY c.committed_at DESC"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query details: %w", err)
	}
	defer rows.Close()

	var result []DetailRow
	for rows.Next() {
		var d DetailRow
		if err := rows.Scan(
			&d.Org, &d.Repo, &d.SHA, &d.AuthorLogin, &d.CommitterLogin, &d.CommittedAt,
			&d.Message, &d.IsBot, &d.IsExemptAuthor, &d.IsEmptyCommit, &d.IsSelfApproved, &d.HasPR, &d.PRNumber,
			&d.PRHref, &d.MergedByLogin, &d.HasFinalApproval, &d.ApproverLogins,
			&d.OwnerApprovalCheck, &d.IsCompliant, &d.Reasons, &d.CommitHref, &d.BranchName,
		); err != nil {
			return nil, fmt.Errorf("scan detail: %w", err)
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// FormatTable writes an ASCII table of summary and details.
func (r *Reporter) FormatTable(w io.Writer, summary []SummaryRow, details []DetailRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	// Summary section
	fmt.Fprintln(tw, "=== SUMMARY ===")
	fmt.Fprintln(tw, "Org\tRepo\tTotal\tCompliant\tNon-Compliant\tBots\tEmpty\tCompliance %")
	for _, s := range summary {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%.1f%%\n",
			s.Org, s.Repo, s.TotalCommits, s.CompliantCount, s.NonCompliantCount,
			s.BotCount, s.EmptyCount, s.CompliancePct)
	}
	fmt.Fprintln(tw)

	// Details section
	fmt.Fprintln(tw, "=== DETAILS ===")
	fmt.Fprintln(tw, "Org\tRepo\tSHA\tAuthor\tMerged By\tDate\tCompliant\tReasons")
	for _, d := range details {
		sha := d.SHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		mergedBy := d.MergedByLogin
		if mergedBy == "" {
			mergedBy = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\n",
			d.Org, d.Repo, sha, d.AuthorLogin, mergedBy,
			d.CommittedAt.Format("2006-01-02 15:04"), d.IsCompliant, d.Reasons)
	}

	return tw.Flush()
}

// FormatCSV writes details as CSV.
func (r *Reporter) FormatCSV(w io.Writer, details []DetailRow) error {
	cw := csv.NewWriter(w)

	header := []string{
		"Org", "Repo", "SHA", "Author", "Merged By", "Date", "Message",
		"Is Bot", "Is Empty", "Has PR", "PR #", "PR Link",
		"Approved", "Approvers", "Owner Approval",
		"Compliant", "Reasons", "Commit Link",
	}
	if err := cw.Write(header); err != nil {
		return err
	}

	for _, d := range details {
		record := []string{
			d.Org, d.Repo, d.SHA, d.AuthorLogin, d.MergedByLogin,
			d.CommittedAt.Format("2006-01-02 15:04:05"),
			d.Message,
			fmt.Sprintf("%v", d.IsBot),
			fmt.Sprintf("%v", d.IsEmptyCommit),
			fmt.Sprintf("%v", d.HasPR),
			fmt.Sprintf("%d", d.PRNumber),
			d.PRHref,
			fmt.Sprintf("%v", d.HasFinalApproval),
			d.ApproverLogins,
			d.OwnerApprovalCheck,
			fmt.Sprintf("%v", d.IsCompliant),
			d.Reasons,
			d.CommitHref,
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}

	cw.Flush()
	return cw.Error()
}

// FormatJSON writes summary and details as JSON.
func (r *Reporter) FormatJSON(w io.Writer, summary []SummaryRow, details []DetailRow) error {
	output := struct {
		Summary []SummaryRow `json:"summary"`
		Details []DetailRow `json:"details"`
	}{
		Summary: summary,
		Details: details,
	}
	if output.Summary == nil {
		output.Summary = []SummaryRow{}
	}
	if output.Details == nil {
		output.Details = []DetailRow{}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

// truncate shortens a string to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}
