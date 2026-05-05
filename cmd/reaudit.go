package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/db"
	"github.com/stefanpenner/gh-audit/internal/model"
	syncer "github.com/stefanpenner/gh-audit/internal/sync"
	"golang.org/x/sync/errgroup"
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
	var concurrency int
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
			"  --repo owner/repo narrow to a specific org/repo (repeatable)\n" +
			"  --concurrency N   parallelize across repos (default 8)\n\n" +
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

			exemptAuthors := append([]model.ExemptAuthor(nil), cfg.Exemptions.Authors...)

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
				concurrency:  concurrency,
			})
		},
	}
	cmd.Flags().BoolVar(&onlyFailures, "only-failures", false,
		"only re-evaluate currently non-compliant rows (narrows 751k→~3k on a typical sweep)")
	cmd.Flags().StringSliceVar(&repoFilter, "repo", nil, "limit to specific org/repo (repeatable)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 8, "number of repos to evaluate in parallel")
	return cmd
}

// reAuditFilter narrows the set of rows re-evaluated. When zero-valued,
// the re-audit scans every repo and every row — the historical behaviour
// before filtering was added.
type reAuditFilter struct {
	onlyFailures bool
	repos        []string
	concurrency  int
}

func runReAudit(ctx context.Context, dbConn *db.DB, logger *slog.Logger, exemptAuthors []model.ExemptAuthor, requiredChecks []syncer.RequiredCheck, filter reAuditFilter) error {
	flipped, total, err := runReAuditPass(ctx, dbConn, logger, exemptAuthors, requiredChecks, filter)
	if err != nil {
		return err
	}
	logger.Info("re-audit pass complete", "commits", total, "flipped", flipped)
	return nil
}

// runReAuditPass re-evaluates every commit once using the current DB state.
// Returns the number of commits whose is_compliant flag changed.
//
// Architecture: per-repo bulk-load of all enrichment data (PRs, reviews,
// check_runs, branch commits, prior classification, prior compliance) into
// in-memory maps via ~7 SQL statements scoped to the repo. The per-commit
// loop then becomes pure CPU: build EnrichmentResult by indexing the maps,
// call EvaluateCommit, accumulate. Concurrency runs across repos via
// errgroup with a bounded limit; DuckDB MVCC handles parallel reads, and
// per-repo writes are serialized through a single mutex to keep the
// bulkUpsert staging-table semantics safe.
func runReAuditPass(
	ctx context.Context,
	dbConn *db.DB,
	logger *slog.Logger,
	exemptAuthors []model.ExemptAuthor,
	requiredChecks []syncer.RequiredCheck,
	filter reAuditFilter,
) (flipped, total int, err error) {
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

	concurrency := filter.concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	var (
		flippedAtomic atomic.Int64
		totalAtomic   atomic.Int64
		writeMu       sync.Mutex
	)

	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(concurrency)
	for _, or := range pairs {
		or := or
		eg.Go(func() error {
			t0 := time.Now()
			bundle, err := loadRepoEnrichmentBundle(gctx, dbConn, or.org, or.repo)
			if err != nil {
				return fmt.Errorf("loading bundle for %s/%s: %w", or.org, or.repo, err)
			}
			loadMs := time.Since(t0).Milliseconds()

			commits, err := loadCandidateCommits(gctx, dbConn, or.org, or.repo, filter)
			if err != nil {
				return fmt.Errorf("loading commits for %s/%s: %w", or.org, or.repo, err)
			}

			tEval := time.Now()
			results := make([]model.AuditResult, 0, len(commits))
			repoFlipped := 0
			for _, c := range commits {
				enrichment := buildEnrichmentFromBundle(c, bundle)
				result := syncer.EvaluateCommit(c, enrichment, exemptAuthors, requiredChecks, nil)
				result.AuditedAt = time.Now()
				results = append(results, result)
				if prior, had := bundle.priorCompliance[c.SHA]; had {
					if prior != result.IsCompliant {
						repoFlipped++
					}
				} else if result.IsCompliant {
					// No prior row existed but the new pass finds this compliant
					// — effectively a flip from "unknown / default non-compliant".
					repoFlipped++
				}
			}
			evalMs := time.Since(tEval).Milliseconds()

			tWrite := time.Now()
			writeMu.Lock()
			defer writeMu.Unlock()
			// DuckDB's INSERT OR REPLACE can't UPDATE LIST columns in place
			// (reasons / approver_logins). When we're rewriting every row in
			// the repo we can DELETE the whole repo's rows and pure-INSERT the
			// batch. When --only-failures (or another filter) narrows the set,
			// per-sha DELETE keeps the other rows untouched.
			if filter.onlyFailures || len(filter.repos) > 0 {
				for _, r := range results {
					if err := dbConn.DeleteAuditResultsBySHA(gctx, r.Org, r.Repo, r.SHA); err != nil {
						return fmt.Errorf("per-sha delete %s/%s@%s: %w", r.Org, r.Repo, r.SHA[:12], err)
					}
				}
			} else {
				if err := dbConn.DeleteAuditResults(gctx, or.org, or.repo); err != nil {
					return fmt.Errorf("clearing audit_results for %s/%s: %w", or.org, or.repo, err)
				}
			}
			if err := dbConn.UpsertAuditResults(gctx, results); err != nil {
				return fmt.Errorf("inserting re-audit results for %s/%s: %w", or.org, or.repo, err)
			}
			if !filter.onlyFailures {
				if err := dbConn.DeleteOrphanedAuditResults(gctx, or.org, or.repo); err != nil {
					return fmt.Errorf("cleaning orphaned audit results for %s/%s: %w", or.org, or.repo, err)
				}
			}
			writeMs := time.Since(tWrite).Milliseconds()

			totalAtomic.Add(int64(len(results)))
			flippedAtomic.Add(int64(repoFlipped))
			logger.Debug("re-audit repo",
				"org", or.org, "repo", or.repo, "commits", len(results),
				"flipped", repoFlipped, "load_ms", loadMs, "eval_ms", evalMs, "write_ms", writeMs)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return int(flippedAtomic.Load()), int(totalAtomic.Load()), err
	}
	return int(flippedAtomic.Load()), int(totalAtomic.Load()), nil
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

// repoEnrichmentBundle holds every row needed to evaluate any commit in a
// single repo. It's populated by loadRepoEnrichmentBundle with ~7 bulk
// queries (independent of commit count) and then indexed in O(1) per commit
// during evaluation. Replaces the per-commit query fan-out that previously
// drove re-audit at ~1M sequential SQL round trips.
type repoEnrichmentBundle struct {
	prsByNumber          map[int]model.PullRequest
	prNumbersByCommit    map[string][]int
	reviewsByPR          map[int][]model.Review
	checkRunsByCommitSHA map[string][]model.CheckRun
	commitsByPR          map[int][]model.Commit
	classByCommit        map[string]db.RevertMergeClassification
	priorCompliance      map[string]bool
}

func loadRepoEnrichmentBundle(ctx context.Context, dbConn *db.DB, org, repo string) (*repoEnrichmentBundle, error) {
	b := &repoEnrichmentBundle{
		prsByNumber:          map[int]model.PullRequest{},
		prNumbersByCommit:    map[string][]int{},
		reviewsByPR:          map[int][]model.Review{},
		checkRunsByCommitSHA: map[string][]model.CheckRun{},
		commitsByPR:          map[int][]model.Commit{},
		classByCommit:        map[string]db.RevertMergeClassification{},
		priorCompliance:      map[string]bool{},
	}

	// 1. pull_requests — same column shape as db.GetPullRequest.
	prRows, err := dbConn.DB.QueryContext(ctx, `
		SELECT org, repo, number, title, merged, head_sha,
		       COALESCE(head_branch, ''), merge_commit_sha, author_login,
		       COALESCE(merged_by_login, ''), merged_at, href
		FROM pull_requests
		WHERE org = ? AND repo = ?`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query pull_requests: %w", err)
	}
	for prRows.Next() {
		var pr model.PullRequest
		if err := prRows.Scan(&pr.Org, &pr.Repo, &pr.Number, &pr.Title, &pr.Merged,
			&pr.HeadSHA, &pr.HeadBranch, &pr.MergeCommitSHA, &pr.AuthorLogin,
			&pr.MergedByLogin, &pr.MergedAt, &pr.Href); err != nil {
			prRows.Close()
			return nil, fmt.Errorf("scan pull_request: %w", err)
		}
		b.prsByNumber[pr.Number] = pr
	}
	prRows.Close()

	// 2. commit_prs — junction; preserve insertion order (matches the
	// ORDER-less GetPRsForCommit which DuckDB returns in storage order).
	cpRows, err := dbConn.DB.QueryContext(ctx,
		"SELECT sha, pr_number FROM commit_prs WHERE org = ? AND repo = ?", org, repo)
	if err != nil {
		return nil, fmt.Errorf("query commit_prs: %w", err)
	}
	for cpRows.Next() {
		var sha string
		var n int
		if err := cpRows.Scan(&sha, &n); err != nil {
			cpRows.Close()
			return nil, fmt.Errorf("scan commit_pr: %w", err)
		}
		b.prNumbersByCommit[sha] = append(b.prNumbersByCommit[sha], n)
	}
	cpRows.Close()

	// 3. reviews — same column shape as GetReviewsForPR; ORDER BY
	// submitted_at preserves the per-PR ordering that the evaluator may
	// rely on for "earliest approval" / "most recent dismissal" semantics.
	rvRows, err := dbConn.DB.QueryContext(ctx, `
		SELECT org, repo, pr_number, review_id, reviewer_login,
		       COALESCE(state::TEXT, ''), commit_id, submitted_at, href
		FROM reviews WHERE org = ? AND repo = ?
		ORDER BY pr_number, submitted_at`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query reviews: %w", err)
	}
	for rvRows.Next() {
		var r model.Review
		if err := rvRows.Scan(&r.Org, &r.Repo, &r.PRNumber, &r.ReviewID, &r.ReviewerLogin,
			&r.State, &r.CommitID, &r.SubmittedAt, &r.Href); err != nil {
			rvRows.Close()
			return nil, fmt.Errorf("scan review: %w", err)
		}
		b.reviewsByPR[r.PRNumber] = append(b.reviewsByPR[r.PRNumber], r)
	}
	rvRows.Close()

	// 4. check_runs — same column shape as GetCheckRunsForCommit.
	crRows, err := dbConn.DB.QueryContext(ctx, `
		SELECT org, repo, commit_sha, check_run_id, check_name,
		       COALESCE(status::TEXT, ''), COALESCE(conclusion::TEXT, ''), completed_at
		FROM check_runs WHERE org = ? AND repo = ?
		ORDER BY commit_sha, check_name`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query check_runs: %w", err)
	}
	for crRows.Next() {
		var cr model.CheckRun
		if err := crRows.Scan(&cr.Org, &cr.Repo, &cr.CommitSHA, &cr.CheckRunID, &cr.CheckName,
			&cr.Status, &cr.Conclusion, &cr.CompletedAt); err != nil {
			crRows.Close()
			return nil, fmt.Errorf("scan check_run: %w", err)
		}
		b.checkRunsByCommitSHA[cr.CommitSHA] = append(b.checkRunsByCommitSHA[cr.CommitSHA], cr)
	}
	crRows.Close()

	// 5. PR-branch commits — commits ⨝ commit_prs scoped to the repo. Same
	// column shape as scanCommits in internal/db.
	cbRows, err := dbConn.DB.QueryContext(ctx, `
		SELECT cp.pr_number,
		       c.org, c.repo, c.sha, c.author_login, c.author_id, c.author_email, c.committer_login,
		       c.committed_at, c.message, c.parent_count, c.additions, c.deletions, c.is_verified, c.href
		FROM commits c
		INNER JOIN commit_prs cp ON c.org = cp.org AND c.repo = cp.repo AND c.sha = cp.sha
		WHERE cp.org = ? AND cp.repo = ?`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query branch commits: %w", err)
	}
	commitIndex := map[string]*model.Commit{} // sha → pointer for co-author attach
	for cbRows.Next() {
		var prNumber int
		var c model.Commit
		var authorID sql.NullInt64
		if err := cbRows.Scan(&prNumber,
			&c.Org, &c.Repo, &c.SHA, &c.AuthorLogin, &authorID, &c.AuthorEmail, &c.CommitterLogin,
			&c.CommittedAt, &c.Message, &c.ParentCount, &c.Additions, &c.Deletions, &c.IsVerified, &c.Href); err != nil {
			cbRows.Close()
			return nil, fmt.Errorf("scan branch commit: %w", err)
		}
		if authorID.Valid {
			c.AuthorID = authorID.Int64
		}
		b.commitsByPR[prNumber] = append(b.commitsByPR[prNumber], c)
		// Track the most-recently-appended pointer per SHA so later
		// co-author attachments mutate every copy. The same SHA can
		// appear under multiple PRs, so we attach in a second pass via
		// SHA → list of indices into the PR slices.
	}
	cbRows.Close()
	_ = commitIndex // index built below in dedicated pass

	// 6. co_authors — bulk per-repo, then attach to every commit copy.
	caRows, err := dbConn.DB.QueryContext(ctx, `
		SELECT sha, COALESCE(name, ''), email, COALESCE(login, '')
		FROM co_authors WHERE org = ? AND repo = ?
		ORDER BY sha, COALESCE(name, ''), email`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query co_authors: %w", err)
	}
	coAuthorsBySHA := map[string][]model.CoAuthor{}
	for caRows.Next() {
		var sha string
		var ca model.CoAuthor
		if err := caRows.Scan(&sha, &ca.Name, &ca.Email, &ca.Login); err != nil {
			caRows.Close()
			return nil, fmt.Errorf("scan co_author: %w", err)
		}
		coAuthorsBySHA[sha] = append(coAuthorsBySHA[sha], ca)
	}
	caRows.Close()
	for prNumber, commits := range b.commitsByPR {
		for i := range commits {
			if cas, ok := coAuthorsBySHA[commits[i].SHA]; ok {
				commits[i].CoAuthors = cas
			}
		}
		b.commitsByPR[prNumber] = commits
	}

	// 7. audit_results — pull both the revert/merge classification
	// (re-audit must preserve, can't recompute offline) AND the prior
	// is_compliant flag (used to count flips). Single statement.
	arRows, err := dbConn.DB.QueryContext(ctx, `
		SELECT sha,
		       COALESCE(is_clean_revert, false),
		       COALESCE(revert_verification, ''),
		       COALESCE(reverted_sha, ''),
		       COALESCE(is_clean_merge, false),
		       COALESCE(merge_verification, ''),
		       is_compliant
		FROM audit_results WHERE org = ? AND repo = ?`, org, repo)
	if err != nil {
		return nil, fmt.Errorf("query audit_results: %w", err)
	}
	for arRows.Next() {
		var sha string
		var c db.RevertMergeClassification
		var isCompliant bool
		if err := arRows.Scan(&sha, &c.IsCleanRevert, &c.RevertVerification, &c.RevertedSHA,
			&c.IsCleanMerge, &c.MergeVerification, &isCompliant); err != nil {
			arRows.Close()
			return nil, fmt.Errorf("scan audit_result: %w", err)
		}
		b.classByCommit[sha] = c
		b.priorCompliance[sha] = isCompliant
	}
	arRows.Close()

	return b, nil
}

// buildEnrichmentFromDB rebuilds an EnrichmentResult for a single commit
// by issuing per-commit DB queries. Used by the backfill path's
// reauditSingleCommit which only ever evaluates one SHA — overhead is
// negligible there. The bulk re-audit hot path uses
// loadRepoEnrichmentBundle / buildEnrichmentFromBundle instead.
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

// buildEnrichmentFromBundle constructs the EnrichmentResult for a single
// commit using only the in-memory bundle — no I/O. Mirrors the shape that
// buildEnrichmentFromDB used to construct via per-commit queries.
func buildEnrichmentFromBundle(commit model.Commit, b *repoEnrichmentBundle) model.EnrichmentResult {
	result := model.EnrichmentResult{
		Commit:          commit,
		PRBranchCommits: map[int][]model.Commit{},
	}
	for _, n := range b.prNumbersByCommit[commit.SHA] {
		pr, ok := b.prsByNumber[n]
		if !ok {
			continue
		}
		result.PRs = append(result.PRs, pr)
		if rs := b.reviewsByPR[n]; len(rs) > 0 {
			result.Reviews = append(result.Reviews, rs...)
		}
		if pr.HeadSHA != "" {
			if runs := b.checkRunsByCommitSHA[pr.HeadSHA]; len(runs) > 0 {
				result.CheckRuns = append(result.CheckRuns, runs...)
			}
		}
		if bc := b.commitsByPR[n]; len(bc) > 0 {
			result.PRBranchCommits[n] = bc
		}
	}
	if c, ok := b.classByCommit[commit.SHA]; ok {
		result.IsCleanRevert = c.IsCleanRevert
		result.RevertVerification = c.RevertVerification
		result.RevertedSHA = c.RevertedSHA
		result.IsCleanMerge = c.IsCleanMerge
		result.MergeVerification = c.MergeVerification
	}
	return result
}
