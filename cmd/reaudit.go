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

func newReAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "re-audit",
		Short: "Re-evaluate audit results using existing enrichment data",
		Long:  "Re-runs the audit decision tree on all commits without fetching from GitHub. Use after updating audit logic.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigOrDefault(cfgFile)

			dbConn, err := db.Open(resolveDBPath(cfg))
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			logger := slog.Default()

			var exemptAuthors []string
			exemptAuthors = append(exemptAuthors, cfg.Exemptions.Authors...)

			var requiredChecks []syncer.RequiredCheck
			for _, rc := range cfg.AuditRules.RequiredChecks {
				requiredChecks = append(requiredChecks, syncer.RequiredCheck{
					Name:       rc.Name,
					Conclusion: rc.Conclusion,
				})
			}

			return runReAudit(cmd.Context(), dbConn, logger, exemptAuthors, requiredChecks)
		},
	}
	return cmd
}

func runReAudit(ctx context.Context, dbConn *db.DB, logger *slog.Logger, exemptAuthors []string, requiredChecks []syncer.RequiredCheck) error {
	// Get all distinct org/repo pairs from commits table
	rows, err := dbConn.DB.QueryContext(ctx, "SELECT DISTINCT org, repo FROM commits ORDER BY org, repo")
	if err != nil {
		return fmt.Errorf("querying org/repo pairs: %w", err)
	}

	type orgRepo struct{ org, repo string }
	var pairs []orgRepo
	for rows.Next() {
		var or orgRepo
		if err := rows.Scan(&or.org, &or.repo); err != nil {
			rows.Close()
			return fmt.Errorf("scanning org/repo: %w", err)
		}
		pairs = append(pairs, or)
	}
	rows.Close()

	totalReaudited := 0
	for _, or := range pairs {
		commits, err := dbConn.GetAllCommits(ctx, or.org, or.repo)
		if err != nil {
			return fmt.Errorf("loading commits for %s/%s: %w", or.org, or.repo, err)
		}

		var results []model.AuditResult
		for _, c := range commits {
			enrichment, err := buildEnrichmentFromDB(ctx, dbConn, or.org, or.repo, c.SHA)
			if err != nil {
				return fmt.Errorf("building enrichment for %s/%s@%s: %w", or.org, or.repo, c.SHA[:12], err)
			}
			enrichment.Commit = c

			result := syncer.EvaluateCommit(c, enrichment, exemptAuthors, requiredChecks)
			result.AuditedAt = time.Now()
			results = append(results, result)
		}

		// Upsert first (INSERT OR REPLACE) so existing rows are preserved on failure.
		if err := dbConn.UpsertAuditResults(ctx, results); err != nil {
			return fmt.Errorf("inserting re-audit results for %s/%s: %w", or.org, or.repo, err)
		}
		// Clean up orphaned audit results for commits that no longer exist.
		if err := dbConn.DeleteOrphanedAuditResults(ctx, or.org, or.repo); err != nil {
			return fmt.Errorf("cleaning orphaned audit results for %s/%s: %w", or.org, or.repo, err)
		}

		logger.Info("re-audited", "org", or.org, "repo", or.repo, "commits", len(results))
		totalReaudited += len(results)
	}

	logger.Info("re-audit complete", "total", totalReaudited)
	return nil
}

func buildEnrichmentFromDB(ctx context.Context, dbConn *db.DB, org, repo, sha string) (model.EnrichmentResult, error) {
	var result model.EnrichmentResult

	prs, err := dbConn.GetPRsForCommit(ctx, org, repo, sha)
	if err != nil {
		return result, err
	}
	result.PRs = prs

	result.PRBranchCommits = make(map[int][]model.Commit)

	for _, pr := range prs {
		reviews, err := dbConn.GetReviewsForPR(ctx, org, repo, pr.Number)
		if err != nil {
			return result, err
		}
		result.Reviews = append(result.Reviews, reviews...)

		if pr.HeadSHA != "" {
			runs, err := dbConn.GetCheckRunsForCommit(ctx, org, repo, pr.HeadSHA)
			if err != nil {
				return result, err
			}
			result.CheckRuns = append(result.CheckRuns, runs...)
		}

		branchCommits, err := dbConn.GetCommitsForPR(ctx, org, repo, pr.Number)
		if err != nil {
			return result, err
		}
		if len(branchCommits) > 0 {
			result.PRBranchCommits[pr.Number] = branchCommits
		}
	}

	return result, nil
}
