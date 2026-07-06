package report

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildManifest(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	// Scope: two commits on main. One compliant via a forgeable exemption,
	// one non-compliant. A third with an unresolved (0) author id.
	insertCommitWithAuthorID(t, db, "org1", "repo1", "c1", "bot", now, 500)
	insertCommitWithAuthorID(t, db, "org1", "repo1", "c2", "dev", now.Add(time.Hour), 600)
	insertCommitWithAuthorID(t, db, "org1", "repo1", "c3", "ghost", now.Add(2*time.Hour), 0)

	insertAuditResultFull(t, db, "org1", "repo1", "c1", auditResultOpts{
		isExempt: true, isCompliant: true, reasons: []string{"exempt: configured author"},
		annotations: []string{"trust:forgeable-exemption"}})
	insertAuditResultFull(t, db, "org1", "repo1", "c2", auditResultOpts{
		hasPR: true, isCompliant: false, prNumber: 1, reasons: []string{"no approval"}})
	insertAuditResultFull(t, db, "org1", "repo1", "c3", auditResultOpts{
		hasPR: true, isCompliant: false, prNumber: 2, reasons: []string{"no approval"}})

	_, err := db.Exec(`INSERT INTO history_rewrites (org, repo, branch, prior_sha, new_sha, compare_status)
		VALUES ('org1','repo1','main','old','new','diverged')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO audit_runs (finished_at, tool_version, config_fingerprint, commits_synced, commits_audited)
		VALUES (?, 'v-audit', 'fp-audit', 3, 3)`, now)
	require.NoError(t, err)

	r := NewWithBranches(db, []string{"master"})
	m, err := r.BuildManifest(ctx, ReportOpts{}, "fp-report", now)
	require.NoError(t, err)

	// Provenance from the audit-run stamp, and drift vs the report config.
	assert.Equal(t, "v-audit", m.ToolVersion)
	assert.Equal(t, "fp-audit", m.ConfigFingerprint)
	assert.Equal(t, "fp-report", m.ReportConfigFingerprint)
	assert.True(t, m.ConfigDrift, "audit fingerprint differs from report fingerprint → drift")

	// Scope.
	assert.Equal(t, []string{"org1/repo1"}, m.Scope.Repos)
	assert.Equal(t, 3, m.Scope.CommitCount)
	assert.Equal(t, now, m.Scope.EarliestCommit.UTC())
	assert.Equal(t, now.Add(2*time.Hour), m.Scope.LatestCommit.UTC())

	// Coverage caveats — the honest disclosure.
	assert.Equal(t, 2, m.Coverage.NonCompliant)
	assert.Equal(t, 1, m.Coverage.ForgeableExemptions)
	assert.Equal(t, 1, m.Coverage.UnresolvedAuthorIDs)
	assert.Equal(t, 1, m.Coverage.HistoryRewrites)

	// Integrity digest: 64-hex, stable across calls, changes when a verdict changes.
	assert.Len(t, m.ResultsDigest, 64)
	m2, err := r.BuildManifest(ctx, ReportOpts{}, "fp-report", now)
	require.NoError(t, err)
	assert.Equal(t, m.ResultsDigest, m2.ResultsDigest, "digest is reproducible over the same DB")

	_, err = db.Exec(`UPDATE audit_results SET is_compliant = true WHERE sha = 'c2'`)
	require.NoError(t, err)
	m3, err := r.BuildManifest(ctx, ReportOpts{}, "fp-report", now)
	require.NoError(t, err)
	assert.NotEqual(t, m.ResultsDigest, m3.ResultsDigest, "tampering with a verdict changes the digest")
}

func TestVerifyResultsDigest(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	insertCommit(t, db, "org1", "repo1", "c1", "dev", now, 1, 0)
	insertAuditResultFull(t, db, "org1", "repo1", "c1", auditResultOpts{
		hasPR: true, hasApproval: true, isCompliant: true, prNumber: 1, reasons: []string{"compliant"}})

	r := NewWithBranches(db, []string{"master"})
	m, err := r.BuildManifest(ctx, ReportOpts{}, "fp", now)
	require.NoError(t, err)

	ok, actual, err := r.VerifyResultsDigest(ctx, ReportOpts{}, m.ResultsDigest)
	require.NoError(t, err)
	assert.True(t, ok, "digest recomputed from the same DB matches the manifest")
	assert.Equal(t, m.ResultsDigest, actual)

	_, err = db.Exec(`UPDATE audit_results SET is_compliant = false WHERE sha = 'c1'`)
	require.NoError(t, err)
	ok, actual, err = r.VerifyResultsDigest(ctx, ReportOpts{}, m.ResultsDigest)
	require.NoError(t, err)
	assert.False(t, ok, "a tampered verdict fails verification")
	assert.NotEqual(t, m.ResultsDigest, actual)
}

func TestBuildManifest_NoDriftWhenFingerprintsMatch(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	insertCommit(t, db, "org1", "repo1", "c1", "dev", now, 1, 0)
	insertAuditResultFull(t, db, "org1", "repo1", "c1", auditResultOpts{
		hasPR: true, hasApproval: true, isCompliant: true, prNumber: 1, reasons: []string{"compliant"}})
	_, err := db.Exec(`INSERT INTO audit_runs (finished_at, tool_version, config_fingerprint, commits_synced, commits_audited)
		VALUES (?, 'v1', 'same-fp', 1, 1)`, now)
	require.NoError(t, err)

	m, err := NewWithBranches(db, []string{"master"}).BuildManifest(ctx, ReportOpts{}, "same-fp", now)
	require.NoError(t, err)
	assert.False(t, m.ConfigDrift, "identical fingerprints → no drift")
}
