package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/db"
	"github.com/stefanpenner/gh-audit/internal/model"
	syncer "github.com/stefanpenner/gh-audit/internal/sync"
)

// newAnnotateCommitsCmd walks every audit_results row, re-computes the
// informational annotations (automation/dep-bump markers, …) from the
// commit's message, and writes them back to the row's `annotations`
// column. No API calls. Idempotent — running twice produces the same DB
// state.
//
// Intended as a one-shot backfill after adding or modifying a detector
// in internal/sync/annotations.go. For new rows, the sync + re-audit
// paths already populate annotations through EvaluateCommit.
func newAnnotateCommitsCmd() *cobra.Command {
	var (
		repoFilter []string
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "annotate-commits",
		Short: "Populate audit_results.annotations from commit messages for every row (no API calls)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigOrDefault(cfgFile)
			dbConn, err := db.Open(resolveDBPath(cfg))
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()
			return runAnnotateCommits(cmd.Context(), dbConn, slog.Default(), repoFilter, dryRun)
		},
	}
	cmd.Flags().StringSliceVar(&repoFilter, "repo", nil, "limit to specific org/repo (repeatable); empty = all repos")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the annotation counts without updating the DB")
	return cmd
}

func runAnnotateCommits(ctx context.Context, dbConn *db.DB, logger *slog.Logger, repoFilter []string, dryRun bool) error {
	args := []any{}
	sqlText := `
SELECT a.org, a.repo, a.sha, c.message
FROM audit_results a
JOIN commits c ON c.org = a.org AND c.repo = a.repo AND c.sha = a.sha
`
	if len(repoFilter) > 0 {
		sqlText += ` WHERE ((a.org || '/' || a.repo) IN (`
		for i, r := range repoFilter {
			if i > 0 {
				sqlText += ", "
			}
			sqlText += "?"
			args = append(args, r)
		}
		sqlText += `))`
	}

	rows, err := dbConn.DB.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return fmt.Errorf("annotate-commits query: %w", err)
	}
	type row struct{ org, repo, sha, message string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.org, &r.repo, &r.sha, &r.message); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	logger.Info("annotate-commits candidates", "count", len(all))

	var withTags, empty int
	tagHistogram := map[string]int{}
	for _, r := range all {
		tags := syncer.ComputeAnnotations(model.Commit{Org: r.org, Repo: r.repo, SHA: r.sha, Message: r.message}, model.EnrichmentResult{})
		if len(tags) == 0 {
			empty++
			if !dryRun {
				// Clear any prior annotations for this row — keeps the column
				// consistent with the current detector output.
				if _, err := dbConn.DB.ExecContext(ctx,
					"UPDATE audit_results SET annotations = NULL WHERE org = ? AND repo = ? AND sha = ?",
					r.org, r.repo, r.sha); err != nil {
					logger.Warn("clear annotations failed", "sha", r.sha[:min(12, len(r.sha))], "error", err)
				}
			}
			continue
		}
		withTags++
		for _, t := range tags {
			tagHistogram[t]++
		}
		if dryRun {
			continue
		}
		// DuckDB's UPDATE on a row with LIST columns internally does
		// DELETE+INSERT and trips the PK constraint, so a plain
		// `UPDATE SET annotations = ...` silently fails to write.
		// Round-trip through UpsertAuditResults (which uses the Appender
		// staging-table path) to replace the whole row with the new
		// annotations field set. Slower, but correct.
		if err := writeAnnotationsForRow(ctx, dbConn, r.org, r.repo, r.sha, tags); err != nil {
			logger.Warn("annotate update failed; skipping", "sha", r.sha[:min(12, len(r.sha))], "error", err)
			continue
		}
	}

	// Top-N histogram for operator visibility.
	type kv struct {
		tag string
		n   int
	}
	top := make([]kv, 0, len(tagHistogram))
	for t, n := range tagHistogram {
		top = append(top, kv{t, n})
	}
	// Simple O(n^2) top-10 print; negligible for the small tag set we expect.
	for i := 0; i < len(top) && i < 10; i++ {
		maxJ := i
		for j := i + 1; j < len(top); j++ {
			if top[j].n > top[maxJ].n {
				maxJ = j
			}
		}
		top[i], top[maxJ] = top[maxJ], top[i]
		logger.Info("annotation histogram", "tag", top[i].tag, "count", top[i].n)
	}
	logger.Info("annotate-commits complete", "rows", len(all), "with_tags", withTags, "empty", empty, "dry_run", dryRun)
	return nil
}

// writeAnnotationsForRow reads the existing audit_results row, mutates
// only the Annotations field, deletes the row, and re-inserts via
// UpsertAuditResults — the Appender-backed staging path that doesn't
// hit DuckDB's UPDATE-on-LIST limitation. All other fields are preserved
// verbatim so this is a true partial update on the annotations column.
func writeAnnotationsForRow(ctx context.Context, dbConn *db.DB, org, repo, sha string, tags []string) error {
	row := dbConn.DB.QueryRowContext(ctx, `
SELECT is_empty_commit, is_bot, is_exempt_author, has_pr, pr_number, pr_count,
       has_final_approval, has_stale_approval, has_post_merge_concern,
       is_clean_revert, COALESCE(revert_verification, ''), COALESCE(reverted_sha, ''),
       is_clean_merge, COALESCE(merge_verification, ''),
       is_self_approved, approver_logins,
       COALESCE(owner_approval_check, ''),
       is_compliant, reasons,
       COALESCE(merge_strategy, ''),
       pr_commit_author_logins,
       COALESCE(commit_href, ''), COALESCE(pr_href, '')
FROM audit_results
WHERE org = ? AND repo = ? AND sha = ?`, org, repo, sha)

	var r model.AuditResult
	r.Org, r.Repo, r.SHA = org, repo, sha
	var approvers, reasons, prAuthors any
	err := row.Scan(
		&r.IsEmptyCommit, &r.IsBot, &r.IsExemptAuthor, &r.HasPR, &r.PRNumber, &r.PRCount,
		&r.HasFinalApproval, &r.HasStaleApproval, &r.HasPostMergeConcern,
		&r.IsCleanRevert, &r.RevertVerification, &r.RevertedSHA,
		&r.IsCleanMerge, &r.MergeVerification,
		&r.IsSelfApproved, &approvers,
		&r.OwnerApprovalCheck,
		&r.IsCompliant, &reasons,
		&r.MergeStrategy,
		&prAuthors,
		&r.CommitHref, &r.PRHref,
	)
	if err != nil {
		return fmt.Errorf("read audit row: %w", err)
	}
	r.ApproverLogins = scanList(approvers)
	r.Reasons = scanList(reasons)
	r.PRCommitAuthorLogins = scanList(prAuthors)
	r.Annotations = tags
	r.AuditedAt = time.Now()

	if err := dbConn.DeleteAuditResultsBySHA(ctx, org, repo, sha); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if err := dbConn.UpsertAuditResults(ctx, []model.AuditResult{r}); err != nil {
		return fmt.Errorf("reinsert: %w", err)
	}
	return nil
}

// scanList converts the driver's any-typed LIST scan result into a []string.
func scanList(v any) []string {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
