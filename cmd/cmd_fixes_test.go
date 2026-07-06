package cmd

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stefanpenner/gh-audit/internal/config"
	"github.com/stefanpenner/gh-audit/internal/db"
	"github.com/stefanpenner/gh-audit/internal/model"
)

// validConfigYAML is the smallest config that passes validation: one org,
// one scoped PAT token.
const validConfigYAML = `
orgs:
  - name: testorg
tokens:
  - kind: pat
    env: GH_AUDIT_TEST_TOKEN
    scopes:
      - org: testorg
`

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Regression: `sync --since not-a-date --org-repos-cache 1h` used to panic
// with a nil-pointer dereference because the error from buildSyncConfig was
// checked only after syncCfg had been dereferenced.
func TestSyncCmd_InvalidSinceReturnsErrorNotPanic(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := writeConfig(t, tmp, validConfigYAML)

	root := NewRootCmd()
	root.SetArgs([]string{"sync",
		"--config", cfgPath,
		"--db", filepath.Join(tmp, "audit.db"),
		"--since", "not-a-date",
		"--org-repos-cache", "1h",
	})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --since")
}

func TestSyncCmd_SinceAfterUntilRejected(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := writeConfig(t, tmp, validConfigYAML)

	root := NewRootCmd()
	root.SetArgs([]string{"sync",
		"--config", cfgPath,
		"--db", filepath.Join(tmp, "audit.db"),
		"--since", "2024-06-01",
		"--until", "2024-01-01",
	})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--since")
	assert.Contains(t, err.Error(), "--until")
}

func TestSyncCmd_OrgAndRepoConflictRejected(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := writeConfig(t, tmp, validConfigYAML)

	root := NewRootCmd()
	root.SetArgs([]string{"sync",
		"--config", cfgPath,
		"--db", filepath.Join(tmp, "audit.db"),
		"--org", "someorg",
		"--repo", "other/repo",
	})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--org")
	assert.Contains(t, err.Error(), "--repo")
}

func TestLoadConfigOrDefault(t *testing.T) {
	tmp := t.TempDir()

	t.Run("missing file without explicit flag falls back to defaults", func(t *testing.T) {
		cfg, err := loadConfigOrDefault(filepath.Join(tmp, "nope.yaml"), false)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Empty(t, cfg.Orgs)
	})

	t.Run("missing file with explicit flag errors", func(t *testing.T) {
		_, err := loadConfigOrDefault(filepath.Join(tmp, "nope.yaml"), true)
		require.Error(t, err)
	})

	t.Run("malformed yaml errors even without explicit flag", func(t *testing.T) {
		path := filepath.Join(tmp, "bad.yaml")
		require.NoError(t, os.WriteFile(path, []byte("orgs: [unclosed"), 0o644))
		_, err := loadConfigOrDefault(path, false)
		require.Error(t, err)
	})

	t.Run("validation failure errors even without explicit flag", func(t *testing.T) {
		path := filepath.Join(tmp, "invalid.yaml")
		// Orgs but no tokens — fails config validation.
		require.NoError(t, os.WriteFile(path, []byte("orgs:\n  - name: x\n"), 0o644))
		_, err := loadConfigOrDefault(path, false)
		require.Error(t, err)
	})

	t.Run("valid file loads", func(t *testing.T) {
		path := writeConfig(t, tmp, validConfigYAML)
		cfg, err := loadConfigOrDefault(path, true)
		require.NoError(t, err)
		require.Len(t, cfg.Orgs, 1)
		assert.Equal(t, "testorg", cfg.Orgs[0].Name)
	})
}

func TestResolveDBPath(t *testing.T) {
	origDBPath := dbPath
	defer func() { dbPath = origDBPath }()

	t.Run("explicit flag wins over config", func(t *testing.T) {
		dbPath = config.DefaultDBPath() // user explicitly typed the default path
		cfg := &config.Config{Database: "/cfg/path.db"}
		assert.Equal(t, config.DefaultDBPath(), resolveDBPath(cfg, true))
	})

	t.Run("config wins when flag not passed", func(t *testing.T) {
		dbPath = config.DefaultDBPath()
		cfg := &config.Config{Database: "/cfg/path.db"}
		assert.Equal(t, "/cfg/path.db", resolveDBPath(cfg, false))
	})

	t.Run("flag default when config empty", func(t *testing.T) {
		dbPath = config.DefaultDBPath()
		cfg := &config.Config{}
		assert.Equal(t, config.DefaultDBPath(), resolveDBPath(cfg, false))
	})
}

func TestBackfillCmd_NegativeWindowDaysRejected(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := writeConfig(t, tmp, validConfigYAML)

	root := NewRootCmd()
	root.SetArgs([]string{"backfill-missing-prs",
		"--config", cfgPath,
		"--db", filepath.Join(tmp, "audit.db"),
		"--window-days", "-1",
	})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--window-days")
}

// Regression: the reclassify candidate query appended the --repo filter as
// `... OR (merge_verification empty) AND repo IN (...)`. SQL precedence
// (AND binds tighter than OR) left the first OR-arm unfiltered, so rows in
// unrelated repos were selected — and then UPDATEd.
func TestRunReclassify_RepoFilterDoesNotLeakAcrossRepos(t *testing.T) {
	tmp := t.TempDir()
	dbConn, err := db.Open(filepath.Join(tmp, "t.db"))
	require.NoError(t, err)
	defer dbConn.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	commits := []model.Commit{
		{Org: "org", Repo: "alpha", SHA: shaA, AuthorID: 1, AuthorLogin: "dev",
			CommittedAt: now, Message: "feat: x", ParentCount: 1},
		{Org: "org", Repo: "beta", SHA: shaB, AuthorID: 1, AuthorLogin: "dev",
			CommittedAt: now, Message: "feat: y", ParentCount: 1},
	}
	require.NoError(t, dbConn.UpsertCommits(ctx, commits))

	// Both rows have empty revert/merge verification — both are reclassify
	// candidates; only repo alpha is allowed by the filter.
	results := []model.AuditResult{
		{Org: "org", Repo: "alpha", SHA: shaA, AuditedAt: now},
		{Org: "org", Repo: "beta", SHA: shaB, AuditedAt: now},
	}
	require.NoError(t, dbConn.UpsertAuditResults(ctx, results))

	require.NoError(t, runReclassify(ctx, dbConn, discardLogger(), false, []string{"org/alpha"}))

	var rvAlpha, rvBeta sql.NullString
	require.NoError(t, dbConn.DB.QueryRowContext(ctx,
		`SELECT revert_verification FROM audit_results WHERE org='org' AND repo='alpha'`).Scan(&rvAlpha))
	require.NoError(t, dbConn.DB.QueryRowContext(ctx,
		`SELECT revert_verification FROM audit_results WHERE org='org' AND repo='beta'`).Scan(&rvBeta))

	assert.Equal(t, "none", rvAlpha.String, "filtered-in repo must be reclassified")
	assert.Empty(t, rvBeta.String, "filtered-out repo must not be touched")
}

// Regression: loadRepoEnrichmentBundle's bulk queries omitted reviewer_id
// (reviews) and author_id/merged_by_id (pull_requests). ID-only matching
// then treated every review as unresolved (ReviewerID==0 → distrusted), so
// a bulk re-audit flipped genuinely approved commits to non-compliant —
// and, symmetrically, could not detect PR-author self-approval.
func TestRunReAuditPass_PreservesReviewIdentityIDs(t *testing.T) {
	tmp := t.TempDir()
	dbConn, err := db.Open(filepath.Join(tmp, "t.db"))
	require.NoError(t, err)
	defer dbConn.Close()
	ctx := context.Background()

	committed := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	merged := committed.Add(time.Hour)

	require.NoError(t, dbConn.UpsertCommits(ctx, []model.Commit{{
		Org: "org", Repo: "r", SHA: "squashsha",
		AuthorID: 10, AuthorLogin: "author", CommitterLogin: "web-flow",
		CommittedAt: committed, Message: "feat: x (#1)",
		ParentCount: 1, Additions: 5, Deletions: 1,
	}}))
	require.NoError(t, dbConn.UpsertPullRequests(ctx, []model.PullRequest{{
		Org: "org", Repo: "r", Number: 1, Merged: true,
		HeadSHA: "headsha", AuthorID: 10, AuthorLogin: "author",
		MergedAt: merged,
	}}))
	require.NoError(t, dbConn.UpsertCommitPRs(ctx, "org", "r", "squashsha", []int{1}))
	require.NoError(t, dbConn.UpsertReviews(ctx, []model.Review{{
		Org: "org", Repo: "r", PRNumber: 1, ReviewID: 500,
		ReviewerID: 20, ReviewerLogin: "reviewer", State: "APPROVED",
		CommitID: "headsha", SubmittedAt: committed.Add(30 * time.Minute),
	}}))

	require.NoError(t, runReAudit(ctx, dbConn, discardLogger(), nil, nil, nil, false,
		reAuditFilter{concurrency: 1}))

	var compliant bool
	require.NoError(t, dbConn.DB.QueryRowContext(ctx,
		`SELECT is_compliant FROM audit_results WHERE org='org' AND repo='r' AND sha='squashsha'`).
		Scan(&compliant))
	assert.True(t, compliant,
		"an independent APPROVED review on the head SHA must survive the bundle round-trip")

	// Self-approval must also survive: the PR author approving their own
	// PR is the only approval here, so the commit must NOT be compliant.
	require.NoError(t, dbConn.UpsertCommits(ctx, []model.Commit{{
		Org: "org", Repo: "r", SHA: "selfsha",
		AuthorID: 11, AuthorLogin: "other", CommitterLogin: "web-flow",
		CommittedAt: committed, Message: "feat: y (#2)",
		ParentCount: 1, Additions: 5, Deletions: 1,
	}}))
	require.NoError(t, dbConn.UpsertPullRequests(ctx, []model.PullRequest{{
		Org: "org", Repo: "r", Number: 2, Merged: true,
		HeadSHA: "headsha2", AuthorID: 30, AuthorLogin: "selfapprover",
		MergedAt: merged,
	}}))
	require.NoError(t, dbConn.UpsertCommitPRs(ctx, "org", "r", "selfsha", []int{2}))
	require.NoError(t, dbConn.UpsertReviews(ctx, []model.Review{{
		Org: "org", Repo: "r", PRNumber: 2, ReviewID: 501,
		ReviewerID: 30, ReviewerLogin: "selfapprover", State: "APPROVED",
		CommitID: "headsha2", SubmittedAt: committed.Add(30 * time.Minute),
	}}))

	require.NoError(t, runReAudit(ctx, dbConn, discardLogger(), nil, nil, nil, false,
		reAuditFilter{concurrency: 1}))

	var selfCompliant, selfApproved bool
	require.NoError(t, dbConn.DB.QueryRowContext(ctx,
		`SELECT is_compliant, is_self_approved FROM audit_results WHERE org='org' AND repo='r' AND sha='selfsha'`).
		Scan(&selfCompliant, &selfApproved))
	assert.False(t, selfCompliant, "PR-author self-approval must not count as independent")
	assert.True(t, selfApproved, "self-approval flag must be preserved through the bundle")
}

// Reclassify must only fill the EMPTY classification family: the OR
// candidate query selects rows where either family is missing, but
// rewriting both destroyed sync-time verification (a diff-verified revert
// with empty merge_verification was downgraded to message-only, killing
// its §8 waiver basis).
func TestRunReclassify_OnlyFillsEmptyFamilies(t *testing.T) {
	tmp := t.TempDir()
	dbConn, err := db.Open(filepath.Join(tmp, "t.db"))
	require.NoError(t, err)
	defer dbConn.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	sha := strings.Repeat("c", 40)
	require.NoError(t, dbConn.UpsertCommits(ctx, []model.Commit{{
		Org: "org", Repo: "r", SHA: sha, AuthorID: 1, AuthorLogin: "dev",
		CommittedAt: now, ParentCount: 2, CommitterLogin: "web-flow", IsVerified: true,
		Message: "Merge pull request #9 from org/branch",
	}}))
	// Sync already diff-verified the revert family; merge family is empty.
	require.NoError(t, dbConn.UpsertAuditResults(ctx, []model.AuditResult{{
		Org: "org", Repo: "r", SHA: sha, AuditedAt: now,
		IsCleanRevert: true, RevertVerification: "diff-verified", RevertedSHA: "f00",
	}}))

	require.NoError(t, runReclassify(ctx, dbConn, discardLogger(), false, nil))

	var rv, mv string
	var cleanRevert bool
	require.NoError(t, dbConn.DB.QueryRowContext(ctx,
		`SELECT COALESCE(revert_verification,''), COALESCE(merge_verification,''), is_clean_revert
		 FROM audit_results WHERE sha = ?`, sha).Scan(&rv, &mv, &cleanRevert))
	assert.Equal(t, "diff-verified", rv, "sync-time revert verification must survive reclassify")
	assert.True(t, cleanRevert)
	assert.Equal(t, "verified-merge-bot", mv,
		"merge family must be filled with the sync-time vocabulary, not a reclassify-only dialect")
}

// verify-reverts must skip AutoRevert rows: the bot's "Automatic revert of
// new..old" message is trusted by construction, and diff-checking it
// against the single parsed SHA can spuriously flip is_clean_revert=false.
// With every candidate filtered out, the client is never consulted.
func TestRunVerifyReverts_SkipsAutoReverts(t *testing.T) {
	tmp := t.TempDir()
	dbConn, err := db.Open(filepath.Join(tmp, "t.db"))
	require.NoError(t, err)
	defer dbConn.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	sha := strings.Repeat("d", 40)
	target := strings.Repeat("e", 40)
	require.NoError(t, dbConn.UpsertCommits(ctx, []model.Commit{{
		Org: "org", Repo: "r", SHA: sha, AuthorID: 1, AuthorLogin: "svc",
		CommittedAt: now, ParentCount: 1,
		Message: "Automatic revert of " + strings.Repeat("a", 40) + ".." + target,
	}}))
	require.NoError(t, dbConn.UpsertAuditResults(ctx, []model.AuditResult{{
		Org: "org", Repo: "r", SHA: sha, AuditedAt: now,
		IsCleanRevert: true, RevertVerification: "message-only", RevertedSHA: target,
		IsCompliant: true, Reasons: []string{"clean revert of " + target[:12]},
	}}))

	// nil client: if the auto-revert were treated as a candidate, the
	// GetCommitFiles call would panic — filtering must happen first.
	require.NoError(t, runVerifyReverts(ctx, dbConn, nil, discardLogger(), false, nil, nil, nil, nil, false))

	var cleanRevert bool
	var rv string
	require.NoError(t, dbConn.DB.QueryRowContext(ctx,
		`SELECT is_clean_revert, revert_verification FROM audit_results WHERE sha = ?`, sha).
		Scan(&cleanRevert, &rv))
	assert.True(t, cleanRevert, "auto-revert trust must not be revoked by diff verification")
	assert.Equal(t, "message-only", rv)
}
