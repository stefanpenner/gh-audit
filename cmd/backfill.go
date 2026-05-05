package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/config"
	"github.com/stefanpenner/gh-audit/internal/db"
	ghclient "github.com/stefanpenner/gh-audit/internal/github"
	"github.com/stefanpenner/gh-audit/internal/model"
	syncer "github.com/stefanpenner/gh-audit/internal/sync"
)

// newBackfillCmd adds a targeted PR-attribution recovery path for commits
// whose original sync produced no PR linkage. The canonical failure mode
// is: /repos/:owner/:repo/commits/:sha/pulls returned either nothing or
// only PRs whose branch happened to contain the commit after the fact.
// For each such commit we enumerate closed PRs on the audit branches in
// a time window around the commit's committed_at, match by
// pull_request.merge_commit_sha, upsert the PR + its reviews + its
// branch commits, then re-audit that commit from DB.
func newBackfillCmd() *cobra.Command {
	var (
		repoFilter     []string
		windowDays     int
		limit          int
		dryRun         bool
		reclassifyOnly bool
		verifyReverts  bool
	)

	cmd := &cobra.Command{
		Use:   "backfill-missing-prs",
		Short: "Find and attach the merging PR for commits currently marked 'no associated pull request'",
		Long: `Walks audit_results rows that are non-compliant with reason "no associated pull request"
and, for each, calls the merge_commit_sha-based canonical lookup against GitHub's PR list API
(time-windowed around the commit's committed_at). On a match, upserts the PR, its reviews,
check runs, and branch commits, then re-audits the commit.

This is a precise, low-volume recovery — it only fires for commits that currently have no
PR attribution, which on a typical sweep is well under 1% of in-scope commits.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigOrDefault(cfgFile)

			dbConn, err := db.Open(resolveDBPath(cfg))
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer dbConn.Close()

			logger := slog.Default()

			// The verify-reverts pass needs API access; reclassify alone does not.
			var client *ghclient.Client
			if !reclassifyOnly || verifyReverts {
				pool, err := buildTokenPool(cfg, logger)
				if err != nil {
					return err
				}
				client = ghclient.NewClient(pool, logger)
			}

			if reclassifyOnly {
				if err := runReclassify(cmd.Context(), dbConn, logger, dryRun, repoFilter); err != nil {
					return err
				}
				if verifyReverts {
					return runVerifyReverts(cmd.Context(), dbConn, client, logger, dryRun, repoFilter)
				}
				return nil
			}

			exemptAuthors := append([]model.ExemptAuthor(nil), cfg.Exemptions.Authors...)

			var requiredChecks []syncer.RequiredCheck
			for _, rc := range cfg.AuditRules.RequiredChecks {
				requiredChecks = append(requiredChecks, syncer.RequiredCheck{Name: rc.Name, Conclusion: rc.Conclusion})
			}

			return runBackfill(cmd.Context(), dbConn, client, cfg, logger, exemptAuthors, requiredChecks, backfillOpts{
				repoFilter: repoFilter,
				windowDays: windowDays,
				limit:      limit,
				dryRun:     dryRun,
			})
		},
	}

	cmd.Flags().StringSliceVar(&repoFilter, "repo", nil, "limit backfill to specific org/repo (repeatable); empty = all repos in DB")
	cmd.Flags().IntVar(&windowDays, "window-days", 14, "time window (in days, each side) around committed_at to scan for the merging PR")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of candidate commits to process (0 = unlimited)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "identify candidates and attempt the lookup, but don't write to the DB")
	cmd.Flags().BoolVar(&reclassifyOnly, "reclassify-only", false,
		"skip PR lookup; walk audit_results rows with empty revert/merge classification and re-derive from commit message (no API calls)")
	cmd.Flags().BoolVar(&verifyReverts, "verify-reverts", false,
		"after reclassify, fetch commit files and upgrade manual-revert rows from 'message-only' to 'diff-verified' / 'diff-mismatch' based on the diff-inverse check (requires API access)")

	return cmd
}

type backfillOpts struct {
	repoFilter []string
	windowDays int
	limit      int
	dryRun     bool
}

type candidateCommit struct {
	org         string
	repo        string
	sha         string
	committedAt time.Time
}

// findNoPRCandidates returns in-scope commits whose current audit_result
// marks them non-compliant specifically because no PR was attached. The
// query filters to audit branches (master/main/release/*/HF_BF_*/hf_bf_*)
// because the compliance rule only applies on those branches; commits on
// feature branches that "have no PR" are not a SOX concern.
func findNoPRCandidates(ctx context.Context, d *db.DB, opts backfillOpts) ([]candidateCommit, error) {
	args := []any{}
	sql := `
SELECT c.org, c.repo, c.sha, c.committed_at
FROM audit_results a
JOIN commits c ON c.org = a.org AND c.repo = a.repo AND c.sha = a.sha
WHERE a.pr_count = 0
  AND a.is_compliant = false
  AND EXISTS (
    SELECT 1 FROM commit_branches cb
    WHERE cb.org = a.org AND cb.repo = a.repo AND cb.sha = a.sha
      AND regexp_matches(cb.branch, '^(master|main|release/.*|HF_BF_.*|hf_bf_.*)$')
  )
`
	if len(opts.repoFilter) > 0 {
		sql += ` AND ((c.org || '/' || c.repo) IN (`
		for i, r := range opts.repoFilter {
			if i > 0 {
				sql += ", "
			}
			sql += "?"
			args = append(args, r)
		}
		sql += `))`
	}
	sql += ` ORDER BY c.committed_at DESC`
	if opts.limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", opts.limit)
	}

	rows, err := d.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("candidate query: %w", err)
	}
	defer rows.Close()

	var out []candidateCommit
	for rows.Next() {
		var c candidateCommit
		if err := rows.Scan(&c.org, &c.repo, &c.sha, &c.committedAt); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// fetchAndPersistPR ingests a single PR (the one FindMergingPR returned)
// plus its reviews, check runs on its head SHA, and branch commits. After
// the writes we link the candidate commit to this PR via commit_prs so
// the re-audit path picks it up.
func fetchAndPersistPR(
	ctx context.Context,
	client *ghclient.Client,
	dbConn *db.DB,
	org, repo, sha string,
	pr *model.PullRequest,
) error {
	// Fetch full PR detail (includes merged_by, final head_sha, etc.).
	fullPR, err := client.GetPullRequest(ctx, org, repo, pr.Number)
	if err != nil {
		return fmt.Errorf("get full PR detail: %w", err)
	}
	if err := dbConn.UpsertPullRequests(ctx, []model.PullRequest{*fullPR}); err != nil {
		return fmt.Errorf("upsert PR: %w", err)
	}

	reviews, err := client.ListReviews(ctx, org, repo, pr.Number)
	if err != nil {
		return fmt.Errorf("list reviews: %w", err)
	}
	if len(reviews) > 0 {
		if err := dbConn.UpsertReviews(ctx, reviews); err != nil {
			return fmt.Errorf("upsert reviews: %w", err)
		}
	}

	if fullPR.HeadSHA != "" {
		runs, err := client.ListCheckRunsForRef(ctx, org, repo, fullPR.HeadSHA)
		if err != nil {
			return fmt.Errorf("list check runs: %w", err)
		}
		if len(runs) > 0 {
			if err := dbConn.UpsertCheckRuns(ctx, runs); err != nil {
				return fmt.Errorf("upsert check runs: %w", err)
			}
		}
	}

	branchCommits, err := client.ListPRCommits(ctx, org, repo, pr.Number)
	if err != nil {
		return fmt.Errorf("list PR commits: %w", err)
	}
	if len(branchCommits) > 0 {
		if err := dbConn.UpsertCommits(ctx, branchCommits); err != nil {
			return fmt.Errorf("upsert PR branch commits: %w", err)
		}
	}

	// Link this commit to its newly-discovered PR.
	if err := dbConn.UpsertCommitPRs(ctx, org, repo, sha, []int{pr.Number}); err != nil {
		return fmt.Errorf("upsert commit_prs link: %w", err)
	}
	return nil
}

func runBackfill(
	ctx context.Context,
	dbConn *db.DB,
	client *ghclient.Client,
	cfg *config.Config,
	logger *slog.Logger,
	exemptAuthors []model.ExemptAuthor,
	requiredChecks []syncer.RequiredCheck,
	opts backfillOpts,
) error {
	candidates, err := findNoPRCandidates(ctx, dbConn, opts)
	if err != nil {
		return err
	}
	logger.Info("backfill candidates", "count", len(candidates))
	if len(candidates) == 0 {
		return nil
	}

	window := time.Duration(opts.windowDays) * 24 * time.Hour
	baseBranches := cfg.AuditRules.AuditBranches
	if len(baseBranches) == 0 {
		baseBranches = []string{"master", "main"}
	}
	// Only concrete (non-glob) branches can be passed to the PR-list API.
	// Glob patterns like "release/*" aren't supported by the base= param;
	// they'd silently return empty, wasting calls. Drop them and rely on
	// the concrete branches that actually cover 99% of cases.
	var concreteBases []string
	for _, b := range baseBranches {
		if !containsGlob(b) {
			concreteBases = append(concreteBases, b)
		}
	}
	if len(concreteBases) == 0 {
		concreteBases = []string{"master", "main"}
	}

	// Group by repo so we can enumerate each repo's closed PRs once instead
	// of re-listing for every candidate. For voyager-android (>98% of our
	// candidates in this DB) that turns ~1,400 × 5-page list calls into a
	// single wider enumeration keyed by merge_commit_sha.
	byRepo := map[string][]candidateCommit{}
	repoOrder := []string{}
	for _, c := range candidates {
		key := c.org + "/" + c.repo
		if _, ok := byRepo[key]; !ok {
			repoOrder = append(repoOrder, key)
		}
		byRepo[key] = append(byRepo[key], c)
	}

	var found, notFound, flipped int
	processed := 0
	for _, rk := range repoOrder {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		group := byRepo[rk]
		c0 := group[0]
		// Window spans all candidates in this repo (± the per-side window).
		earliest, latest := c0.committedAt, c0.committedAt
		for _, c := range group {
			if c.committedAt.Before(earliest) {
				earliest = c.committedAt
			}
			if c.committedAt.After(latest) {
				latest = c.committedAt
			}
		}
		index, err := buildRepoPRIndex(ctx, client, c0.org, c0.repo, concreteBases, earliest.Add(-window), latest.Add(window), logger)
		if err != nil {
			logger.Warn("repo PR enumeration failed; candidates in this repo will be skipped",
				"repo", rk, "error", err, "candidates", len(group))
			notFound += len(group)
			processed += len(group)
			continue
		}
		logger.Info("enumerated repo PRs", "repo", rk, "prs_indexed", len(index), "candidates", len(group))

		for _, c := range group {
			processed++
			pr, ok := index[sanitizeSHA(c.sha)]
			if !ok {
				notFound++
				continue
			}
			found++
			logger.Info("found merging PR",
				"org", c.org, "repo", c.repo, "sha", c.sha[:12],
				"pr", pr.Number, "merged_at", pr.MergedAt,
			)
			if opts.dryRun {
				continue
			}
			if err := persistBackfilledPR(ctx, client, dbConn, c.org, c.repo, c.sha, pr); err != nil {
				logger.Error("persisting PR failed", "org", c.org, "repo", c.repo, "sha", c.sha[:12], "pr", pr.Number, "error", err)
				continue
			}
			// Preserve the enrichment-phase revert / merge classification
			// across the re-audit. buildEnrichmentFromDB doesn't populate
			// those fields (they require GetCommitFiles against the
			// reverted commit, which re-audit skips), so without this
			// read-then-reapply step the re-audit would zero them out.
			priorClass, err := dbConn.GetRevertMergeClassification(ctx, c.org, c.repo, c.sha)
			if err != nil {
				logger.Warn("couldn't read prior classification; re-audit will zero revert/merge fields",
					"org", c.org, "repo", c.repo, "sha", c.sha[:12], "error", err)
			}
			if err := dbConn.DeleteAuditResultsBySHA(ctx, c.org, c.repo, c.sha); err != nil {
				logger.Error("delete prior audit result failed", "error", err)
				continue
			}
			newResults, err := reauditSingleCommit(ctx, dbConn, c.org, c.repo, c.sha, exemptAuthors, requiredChecks)
			if err != nil {
				logger.Error("re-audit failed", "error", err)
				continue
			}
			// Copy the preserved classification onto the re-audited row.
			newResults[0].IsCleanRevert = priorClass.IsCleanRevert
			newResults[0].RevertVerification = priorClass.RevertVerification
			newResults[0].RevertedSHA = priorClass.RevertedSHA
			newResults[0].IsCleanMerge = priorClass.IsCleanMerge
			newResults[0].MergeVerification = priorClass.MergeVerification
			if err := dbConn.UpsertAuditResults(ctx, newResults); err != nil {
				logger.Error("upsert audit result failed", "error", err)
				continue
			}
			if newResults[0].IsCompliant {
				flipped++
			}
			if processed%50 == 0 {
				logger.Info("backfill progress", "processed", processed, "total", len(candidates), "found", found, "flipped", flipped)
			}
		}
	}

	logger.Info("backfill complete",
		"candidates", len(candidates),
		"found_pr", found,
		"no_pr_in_window", notFound,
		"flipped_to_compliant", flipped,
		"dry_run", opts.dryRun,
	)
	return nil
}

// buildRepoPRIndex enumerates every merged PR on `repo`'s audit-base branches
// whose merged_at falls within [windowStart, windowEnd], and returns a map
// keyed by lower-case merge_commit_sha → PR. One pre-fetch here replaces one
// FindMergingPR call per candidate.
func buildRepoPRIndex(
	ctx context.Context,
	client *ghclient.Client,
	org, repo string,
	bases []string,
	windowStart, windowEnd time.Time,
	logger *slog.Logger,
) (map[string]*model.PullRequest, error) {
	out := make(map[string]*model.PullRequest)
	// FindMergingPR already handles the base-loop + pagination + window
	// trimming we want; call it with a wildcard target SHA ("") so it
	// walks the full page range and we capture every PR it sees via a
	// scraper callback. Rather than duplicate that logic, enumerate
	// directly here — it's a short loop.
	for _, base := range bases {
		if err := enumerateClosedPRs(ctx, client, org, repo, base, windowStart, windowEnd, func(pr *model.PullRequest) {
			if pr.MergeCommitSHA == "" {
				return
			}
			out[sanitizeSHA(pr.MergeCommitSHA)] = pr
		}); err != nil {
			return out, err
		}
	}
	return out, nil
}

// persistBackfilledPR writes a PR (plus its reviews, check runs, and branch
// commits) into the DB and links the candidate commit to it. Mirrors
// fetchAndPersistPR but reuses the PR we already have from the index — no
// extra GetPullRequest call needed (the list endpoint returned every field
// we persist).
func persistBackfilledPR(
	ctx context.Context,
	client *ghclient.Client,
	dbConn *db.DB,
	org, repo, sha string,
	pr *model.PullRequest,
) error {
	if err := dbConn.UpsertPullRequests(ctx, []model.PullRequest{*pr}); err != nil {
		return fmt.Errorf("upsert PR: %w", err)
	}
	reviews, err := client.ListReviews(ctx, org, repo, pr.Number)
	if err != nil {
		return fmt.Errorf("list reviews: %w", err)
	}
	if len(reviews) > 0 {
		if err := dbConn.UpsertReviews(ctx, reviews); err != nil {
			return fmt.Errorf("upsert reviews: %w", err)
		}
	}
	if pr.HeadSHA != "" {
		runs, err := client.ListCheckRunsForRef(ctx, org, repo, pr.HeadSHA)
		if err != nil {
			return fmt.Errorf("list check runs: %w", err)
		}
		if len(runs) > 0 {
			if err := dbConn.UpsertCheckRuns(ctx, runs); err != nil {
				return fmt.Errorf("upsert check runs: %w", err)
			}
		}
	}
	branchCommits, err := client.ListPRCommits(ctx, org, repo, pr.Number)
	if err != nil {
		return fmt.Errorf("list PR commits: %w", err)
	}
	if len(branchCommits) > 0 {
		if err := dbConn.UpsertCommits(ctx, branchCommits); err != nil {
			return fmt.Errorf("upsert PR branch commits: %w", err)
		}
	}
	if err := dbConn.UpsertCommitPRs(ctx, org, repo, sha, []int{pr.Number}); err != nil {
		return fmt.Errorf("upsert commit_prs link: %w", err)
	}
	return nil
}

func sanitizeSHA(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// enumerateClosedPRs paginates GitHub's PR list with the supplied base,
// calling back on each merged PR whose merged_at falls in the window. The
// pagination halts once a page's youngest-updated PR drops below the window
// — older pages can't contain a useful match.
func enumerateClosedPRs(
	ctx context.Context,
	client *ghclient.Client,
	org, repo, base string,
	windowStart, windowEnd time.Time,
	cb func(pr *model.PullRequest),
) error {
	return client.ListClosedMergedPRs(ctx, org, repo, base, windowStart, windowEnd, cb)
}

// Delete the older fetchAndPersistPR path; the new index-based flow uses
// persistBackfilledPR directly.
var _ = fetchAndPersistPR

// reauditSingleCommit rebuilds the enrichment snapshot for one commit from
// the DB and runs EvaluateCommit against it. Mirrors the shape of the
// bulk re-audit path but scoped to a single SHA.
func reauditSingleCommit(
	ctx context.Context,
	dbConn *db.DB,
	org, repo, sha string,
	exemptAuthors []model.ExemptAuthor,
	requiredChecks []syncer.RequiredCheck,
) ([]model.AuditResult, error) {
	commits, err := dbConn.GetCommitsBySHA(ctx, org, repo, []string{sha})
	if err != nil || len(commits) == 0 {
		return nil, fmt.Errorf("commit not in DB: %s/%s@%s", org, repo, sha[:12])
	}
	c := commits[0]
	enrichment, err := buildEnrichmentFromDB(ctx, dbConn, org, repo, sha)
	if err != nil {
		return nil, err
	}
	enrichment.Commit = c
	result := syncer.EvaluateCommit(c, enrichment, exemptAuthors, requiredChecks, nil)
	result.AuditedAt = time.Now()
	return []model.AuditResult{result}, nil
}

// runReclassify walks audit_results rows whose revert_verification is empty
// — a tell-tale of rows that went through re-audit before buildEnrichmentFromDB
// was taught to preserve the enrichment-phase classification — and re-derives
// the revert / merge classification from the commit message + parent count.
// No API calls: "diff-verified" manual reverts downgrade to "message-only"
// because we can't re-run the diff check offline. Operators who want the
// full diff-verified label can re-run a targeted sync afterwards.
func runReclassify(
	ctx context.Context,
	dbConn *db.DB,
	logger *slog.Logger,
	dryRun bool,
	repoFilter []string,
) error {
	args := []any{}
	sql := `
SELECT a.org, a.repo, a.sha, c.message, c.parent_count, c.committer_login, c.is_verified
FROM audit_results a
JOIN commits c ON c.org=a.org AND c.repo=a.repo AND c.sha=a.sha
WHERE (a.revert_verification IS NULL OR a.revert_verification = '')
   OR (a.merge_verification  IS NULL OR a.merge_verification  = '')
`
	if len(repoFilter) > 0 {
		sql += ` AND ((a.org || '/' || a.repo) IN (`
		for i, r := range repoFilter {
			if i > 0 {
				sql += ", "
			}
			sql += "?"
			args = append(args, r)
		}
		sql += `))`
	}
	rows, err := dbConn.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("reclassify query: %w", err)
	}
	defer rows.Close()

	type reclassifyRow struct {
		org, repo, sha, message, committerLogin string
		parentCount                             int
		isVerified                              bool
	}
	var candidates []reclassifyRow
	for rows.Next() {
		var r reclassifyRow
		if err := rows.Scan(&r.org, &r.repo, &r.sha, &r.message, &r.parentCount, &r.committerLogin, &r.isVerified); err != nil {
			return fmt.Errorf("scan reclassify row: %w", err)
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	logger.Info("reclassify candidates", "count", len(candidates))

	var updated, unchanged int
	for _, r := range candidates {
		kind, revertedSHA := ghclient.ParseRevert(r.message)
		var isCleanRevert bool
		var revertVerification string
		switch kind {
		case ghclient.NotRevert, ghclient.RevertOfRevert:
			revertVerification = "none"
		case ghclient.AutoRevert:
			isCleanRevert = true
			revertVerification = "message-only"
		case ghclient.ManualRevert:
			// Offline re-derivation can't run the diff-verify check; fall
			// back to the message-only label (same thing enrichOneCommit
			// would have stored if GetCommitFiles had failed at sync time).
			revertVerification = "message-only"
		}

		mk := ghclient.ClassifyMerge(r.parentCount, r.message, r.committerLogin, r.isVerified)
		isCleanMerge := mk == ghclient.CleanMerge
		mergeVerification := mergeKindToVerification(mk)

		if dryRun {
			updated++
			continue
		}

		res, err := dbConn.DB.ExecContext(ctx, `
UPDATE audit_results
SET is_clean_revert = ?,
    revert_verification = ?,
    reverted_sha = ?,
    is_clean_merge = ?,
    merge_verification = ?
WHERE org = ? AND repo = ? AND sha = ?`,
			isCleanRevert, revertVerification, revertedSHA,
			isCleanMerge, mergeVerification,
			r.org, r.repo, r.sha)
		if err != nil {
			logger.Warn("reclassify update failed", "sha", r.sha[:12], "error", err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
		} else {
			unchanged++
		}
	}
	logger.Info("reclassify complete", "updated", updated, "unchanged", unchanged, "dry_run", dryRun)
	return nil
}

// runVerifyReverts upgrades rows currently at revert_verification='message-only'
// (manual reverts with a known target SHA) to 'diff-verified' or 'diff-mismatch'
// by fetching both commits' files and running the same diff-inverse check
// enrichOneCommit does at sync time. Idempotent — re-running is a no-op
// because rows past message-only are skipped.
func runVerifyReverts(
	ctx context.Context,
	dbConn *db.DB,
	client *ghclient.Client,
	logger *slog.Logger,
	dryRun bool,
	repoFilter []string,
) error {
	args := []any{}
	sql := `
SELECT a.org, a.repo, a.sha, a.reverted_sha
FROM audit_results a
WHERE a.revert_verification = 'message-only'
  AND a.reverted_sha IS NOT NULL AND a.reverted_sha <> ''`
	if len(repoFilter) > 0 {
		sql += ` AND ((a.org || '/' || a.repo) IN (`
		for i, r := range repoFilter {
			if i > 0 {
				sql += ", "
			}
			sql += "?"
			args = append(args, r)
		}
		sql += `))`
	}
	rows, err := dbConn.DB.QueryContext(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("verify-reverts query: %w", err)
	}
	type row struct{ org, repo, sha, revertedSHA string }
	var candidates []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.org, &r.repo, &r.sha, &r.revertedSHA); err != nil {
			rows.Close()
			return fmt.Errorf("scan verify-reverts row: %w", err)
		}
		candidates = append(candidates, r)
	}
	rows.Close()
	logger.Info("verify-reverts candidates", "count", len(candidates))

	var verified, mismatch, fetchErrors int
	for i, r := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		outer, err := client.GetCommitFiles(ctx, r.org, r.repo, r.sha)
		if err != nil {
			fetchErrors++
			logger.Warn("outer commit files fetch failed; skipping",
				"sha", r.sha[:12], "error", err)
			continue
		}
		inner, err := client.GetCommitFiles(ctx, r.org, r.repo, r.revertedSHA)
		if err != nil {
			fetchErrors++
			logger.Warn("reverted commit files fetch failed; skipping",
				"sha", r.sha[:12], "reverted_sha", r.revertedSHA[:12], "error", err)
			continue
		}
		isClean := ghclient.IsCleanRevertDiff(outer, inner)
		verification := "diff-mismatch"
		if isClean {
			verification = "diff-verified"
			verified++
		} else {
			mismatch++
		}
		if dryRun {
			continue
		}
		if _, err := dbConn.DB.ExecContext(ctx, `
UPDATE audit_results
SET is_clean_revert = ?, revert_verification = ?
WHERE org = ? AND repo = ? AND sha = ?`,
			isClean, verification, r.org, r.repo, r.sha); err != nil {
			logger.Warn("verify-reverts update failed", "sha", r.sha[:12], "error", err)
			continue
		}
		if (i+1)%100 == 0 {
			logger.Info("verify-reverts progress",
				"processed", i+1, "total", len(candidates),
				"verified", verified, "mismatch", mismatch, "fetch_errors", fetchErrors)
		}
	}

	logger.Info("verify-reverts complete",
		"candidates", len(candidates),
		"diff_verified", verified,
		"diff_mismatch", mismatch,
		"fetch_errors", fetchErrors,
		"dry_run", dryRun,
	)
	return nil
}

// mergeKindToVerification mirrors enrichOneCommit's classifier mapping so
// the reclassify path produces identical merge_verification strings.
func mergeKindToVerification(mk ghclient.MergeKind) string {
	switch mk {
	case ghclient.NotMerge:
		return "none"
	case ghclient.CleanMerge:
		return "message-only"
	case ghclient.DirtyMerge:
		return "dirty"
	case ghclient.OctopusMerge:
		return "octopus"
	}
	return "none"
}

// containsGlob returns true iff s uses glob syntax the PR-list API can't
// resolve (we only support concrete branch names when probing).
func containsGlob(s string) bool {
	for _, r := range s {
		if r == '*' || r == '?' || r == '[' {
			return true
		}
	}
	return false
}
