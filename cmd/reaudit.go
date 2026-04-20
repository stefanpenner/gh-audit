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

// newReAuditCmd returns the primary "re-evaluate-commits" command. Its
// prior name was "re-audit"; an alias is registered alongside so existing
// scripts keep working while operators migrate. The rename makes the
// narrower scope explicit — this command re-runs the compliance
// *evaluation* over existing DB state, it does NOT re-run the sync or
// re-classify revert/merge signals. See the Long description for the
// boundary.
func newReAuditCmd() *cobra.Command {
	var onlyFailures bool
	var repoFilter []string
	cmd := &cobra.Command{
		Use:     "re-evaluate-commits",
		Aliases: []string{"re-audit"},
		Short:   "Re-run compliance evaluation over existing DB state (no GitHub calls)",
		Long: "Re-runs EvaluateCommit on every audit_results row using the data already in the DB.\n" +
			"Useful after you change audit logic (new rule, bug fix, policy tweak) and want existing\n" +
			"rows to reflect it without a fresh sweep.\n\n" +
			"Single pass: every revert is evaluated standalone (no cross-commit lookup), so no\n" +
			"fixed-point iteration is needed. See TODO.md for the deferred cross-commit variant.\n\n" +
			"Flags:\n" +
			"  --only-failures   limit to currently non-compliant rows (100× faster when a fix can\n" +
			"                    only flip things in the non-compliant direction → compliant)\n" +
			"  --repo owner/repo narrow to a specific org/repo (repeatable)\n\n" +
			"Does NOT:\n" +
			"  • call GitHub (no new commits, PRs, reviews, or checks are discovered)\n" +
			"  • recompute clean-revert diff verification (that check needs GetCommitFiles against\n" +
			"    the reverted commit at sync time; re-evaluation preserves whatever the sync stored)\n" +
			"  • populate commit-message annotations (use `annotate-commits` for that)\n\n" +
			"The legacy alias `re-audit` remains wired to this same command.",
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

			return runReAudit(cmd.Context(), dbConn, logger, exemptAuthors, requiredChecks, reAuditFilter{
				onlyFailures: onlyFailures,
				repos:        repoFilter,
			})
		},
	}
	cmd.Flags().BoolVar(&onlyFailures, "only-failures", false,
		"only re-evaluate currently non-compliant rows (narrows 751k→~3k on a typical sweep)")
	cmd.Flags().StringSliceVar(&repoFilter, "repo", nil, "limit to specific org/repo (repeatable)")
	return cmd
}

// reAuditFilter narrows the set of rows re-evaluated. When zero-valued,
// the re-audit scans every repo and every row — the historical behaviour
// before filtering was added.
type reAuditFilter struct {
	onlyFailures bool
	repos        []string
}

func runReAudit(ctx context.Context, dbConn *db.DB, logger *slog.Logger, exemptAuthors []string, requiredChecks []syncer.RequiredCheck, filter reAuditFilter) error {
	flipped, total, err := runReAuditPass(ctx, dbConn, logger, exemptAuthors, requiredChecks, filter)
	if err != nil {
		return err
	}
	logger.Info("re-audit pass complete", "commits", total, "flipped", flipped)
	return nil
}

// runReAuditPass re-evaluates every commit once using the current DB state.
// Returns the number of commits whose is_compliant flag changed.
func runReAuditPass(
	ctx context.Context,
	dbConn *db.DB,
	logger *slog.Logger,
	exemptAuthors []string,
	requiredChecks []syncer.RequiredCheck,
	filter reAuditFilter,
) (flipped, total int, err error) {
	priorCompliance := func(org, repo, sha string) (bool, bool) {
		var isCompliant bool
		if err := dbConn.DB.QueryRowContext(ctx,
			"SELECT is_compliant FROM audit_results WHERE org = ? AND repo = ? AND sha = ?",
			org, repo, sha).Scan(&isCompliant); err != nil {
			return false, false
		}
		return isCompliant, true
	}
	rows, err := dbConn.DB.QueryContext(ctx, "SELECT DISTINCT org, repo FROM commits ORDER BY org, repo")
	if err != nil {
		return 0, 0, fmt.Errorf("querying org/repo pairs: %w", err)
	}

	type orgRepo struct{ org, repo string }
	var pairs []orgRepo
	repoAllow := map[string]struct{}{}
	for _, r := range filter.repos {
		repoAllow[r] = struct{}{}
	}
	for rows.Next() {
		var or orgRepo
		if err := rows.Scan(&or.org, &or.repo); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scanning org/repo: %w", err)
		}
		if len(repoAllow) > 0 {
			if _, ok := repoAllow[or.org+"/"+or.repo]; !ok {
				continue
			}
		}
		pairs = append(pairs, or)
	}
	rows.Close()

	for _, or := range pairs {
		commits, err := loadCandidateCommits(ctx, dbConn, or.org, or.repo, filter)
		if err != nil {
			return flipped, total, fmt.Errorf("loading commits for %s/%s: %w", or.org, or.repo, err)
		}

		var results []model.AuditResult
		for _, c := range commits {
			enrichment, err := buildEnrichmentFromDB(ctx, dbConn, or.org, or.repo, c.SHA)
			if err != nil {
				return flipped, total, fmt.Errorf("building enrichment for %s/%s@%s: %w", or.org, or.repo, c.SHA[:12], err)
			}
			enrichment.Commit = c

			priorCompliant, hadPrior := priorCompliance(or.org, or.repo, c.SHA)

			// re-audit runs off existing DB state only — no API fallback
			// for stats. Each commit is evaluated standalone.
			result := syncer.EvaluateCommit(c, enrichment, exemptAuthors, requiredChecks, nil)
			result.AuditedAt = time.Now()
			results = append(results, result)
			total++
			if hadPrior && priorCompliant != result.IsCompliant {
				flipped++
			} else if !hadPrior && result.IsCompliant {
				// No prior row existed but the new pass finds this compliant
				// — effectively a flip from "unknown / default non-compliant".
				flipped++
			}
		}

		// DuckDB's INSERT OR REPLACE can't UPDATE LIST columns in place
		// (reasons / approver_logins). When we're rewriting every row in
		// the repo we can DELETE the whole repo's rows and pure-INSERT the
		// batch. When --only-failures (or another filter) narrows the set,
		// per-sha DELETE keeps the other rows untouched.
		if filter.onlyFailures || len(filter.repos) > 0 {
			for _, r := range results {
				if err := dbConn.DeleteAuditResultsBySHA(ctx, r.Org, r.Repo, r.SHA); err != nil {
					return flipped, total, fmt.Errorf("per-sha delete %s/%s@%s: %w", r.Org, r.Repo, r.SHA[:12], err)
				}
			}
		} else {
			if err := dbConn.DeleteAuditResults(ctx, or.org, or.repo); err != nil {
				return flipped, total, fmt.Errorf("clearing audit_results for %s/%s: %w", or.org, or.repo, err)
			}
		}
		if err := dbConn.UpsertAuditResults(ctx, results); err != nil {
			return flipped, total, fmt.Errorf("inserting re-audit results for %s/%s: %w", or.org, or.repo, err)
		}
		if !filter.onlyFailures {
			// Orphan cleanup only makes sense when we've rewritten the whole repo.
			if err := dbConn.DeleteOrphanedAuditResults(ctx, or.org, or.repo); err != nil {
				return flipped, total, fmt.Errorf("cleaning orphaned audit results for %s/%s: %w", or.org, or.repo, err)
			}
		}
		logger.Debug("re-audit repo", "org", or.org, "repo", or.repo, "commits", len(results))
	}
	return flipped, total, nil
}

// loadCandidateCommits narrows the set of commits to re-evaluate per the
// supplied filter. When the filter is empty this is just GetAllCommits (the
// historical behaviour). With --only-failures it joins audit_results to
// keep only currently non-compliant rows.
func loadCandidateCommits(ctx context.Context, dbConn *db.DB, org, repo string, filter reAuditFilter) ([]model.Commit, error) {
	if !filter.onlyFailures {
		return dbConn.GetAllCommits(ctx, org, repo)
	}
	rows, err := dbConn.DB.QueryContext(ctx, `
SELECT c.org, c.repo, c.sha, COALESCE(c.author_login, ''), COALESCE(c.author_email, ''),
       COALESCE(c.committer_login, ''), c.committed_at, COALESCE(c.message, ''),
       COALESCE(c.parent_count, 0), COALESCE(c.additions, 0), COALESCE(c.deletions, 0),
       COALESCE(c.href, '')
FROM commits c
JOIN audit_results a ON a.org = c.org AND a.repo = c.repo AND a.sha = c.sha
WHERE c.org = ? AND c.repo = ? AND a.is_compliant = false
ORDER BY c.committed_at`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query failing commits: %w", err)
	}
	defer rows.Close()
	var out []model.Commit
	for rows.Next() {
		var c model.Commit
		if err := rows.Scan(&c.Org, &c.Repo, &c.SHA, &c.AuthorLogin, &c.AuthorEmail,
			&c.CommitterLogin, &c.CommittedAt, &c.Message,
			&c.ParentCount, &c.Additions, &c.Deletions, &c.Href); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
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

	// Re-audit can't recompute revert / merge classification (the
	// diff-verify check requires GetCommitFiles on the reverted commit,
	// an API call re-audit intentionally skips). Preserve the stored
	// classification so EvaluateCommit's revert waivers (R1 clean revert,
	// R2 GitHub-server revert) have the signals they need.
	priorClass, err := dbConn.GetRevertMergeClassification(ctx, org, repo, sha)
	if err == nil {
		result.IsCleanRevert = priorClass.IsCleanRevert
		result.RevertVerification = priorClass.RevertVerification
		result.RevertedSHA = priorClass.RevertedSHA
		result.IsCleanMerge = priorClass.IsCleanMerge
		result.MergeVerification = priorClass.MergeVerification
	}

	return result, nil
}
