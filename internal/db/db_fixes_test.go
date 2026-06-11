package db

import (
	"context"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: DuckDB's INSERT OR REPLACE cannot replace a row whose existing
// version has non-empty LIST columns ("List Update is not supported"). Nearly
// every audit row has non-empty reasons or approver_logins, so re-upserting
// (multi-branch sync, re-audit) hard-failed.
func TestUpsertAuditResults_ReplacesRowWithListColumns(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	first := model.AuditResult{
		Org: "o", Repo: "r", SHA: "abc",
		IsCompliant: false,
		Reasons:     []string{"no associated pull request"},
		AuditedAt:   time.Now(),
	}
	require.NoError(t, db.UpsertAuditResults(ctx, []model.AuditResult{first}))

	second := first
	second.IsCompliant = true
	second.Reasons = []string{"compliant"}
	second.ApproverLogins = []string{"reviewer1"}
	require.NoError(t, db.UpsertAuditResults(ctx, []model.AuditResult{second}),
		"second upsert over a row with non-empty LIST columns must not fail")

	rows, err := db.GetAuditResults(ctx, AuditQueryOpts{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].IsCompliant)
}

// Intra-batch duplicates must resolve last-wins deterministically (the
// QUALIFY dedup previously had no ORDER BY, leaving the survivor arbitrary).
func TestUpsertAuditResults_IntraBatchDuplicateLastWins(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	batch := []model.AuditResult{
		{Org: "o", Repo: "r", SHA: "dup", IsCompliant: false,
			Reasons: []string{"first version"}, AuditedAt: time.Now()},
		{Org: "o", Repo: "r", SHA: "dup", IsCompliant: true,
			Reasons: []string{"second version"}, AuditedAt: time.Now()},
	}
	for i := 0; i < 5; i++ { // run several times: arbitrary-order bugs are flaky
		require.NoError(t, db.UpsertAuditResults(ctx, batch))
		rows, err := db.GetAuditResults(ctx, AuditQueryOpts{})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.True(t, rows[0].IsCompliant, "last row in the batch must win (iteration %d)", i)
	}
}

// Legacy DBs created before the committer_login column have NULL in that
// column for old rows; every commit read path must tolerate it.
func TestCommitReads_TolerateNullCommitterLogin(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO commits (org, repo, sha, author_login, author_id, author_email,
		                     committer_login, committed_at, message, parent_count,
		                     additions, deletions, href)
		VALUES ('o', 'r', 'legacy', 'alice', 1, 'a@x.com',
		        NULL, TIMESTAMP '2024-01-01 00:00:00', 'old row', 1, 1, 1, '')`)
	require.NoError(t, err)

	all, err := db.GetAllCommits(ctx, "o", "r")
	require.NoError(t, err, "GetAllCommits must tolerate NULL committer_login")
	require.Len(t, all, 1)
	assert.Empty(t, all[0].CommitterLogin)

	unaudited, err := db.GetUnauditedCommits(ctx, "o", "r", time.Time{}, time.Time{})
	require.NoError(t, err, "GetUnauditedCommits must tolerate NULL committer_login")
	require.Len(t, unaudited, 1)

	bySHA, err := db.GetCommitsBySHA(ctx, "o", "r", []string{"legacy"})
	require.NoError(t, err, "GetCommitsBySHA must tolerate NULL committer_login")
	require.Len(t, bySHA, 1)

	require.NoError(t, db.UpsertCommitPRs(ctx, "o", "r", "legacy", []int{7}))
	forPR, err := db.GetCommitsForPR(ctx, "o", "r", 7)
	require.NoError(t, err, "GetCommitsForPR must tolerate NULL committer_login")
	require.Len(t, forPR, 1)
}

// GitHub returns PENDING for the caller's own draft reviews, and may add new
// states; the old review_state ENUM made the whole batch (and the branch
// sync) hard-fail. PENDING is filtered (a draft is not an audit event);
// unknown future states are stored as-is.
func TestUpsertReviews_PendingFilteredAndUnknownStateStored(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	reviews := []model.Review{
		{Org: "o", Repo: "r", PRNumber: 1, ReviewID: 1, ReviewerLogin: "a",
			State: "APPROVED", CommitID: "c1", SubmittedAt: time.Now()},
		{Org: "o", Repo: "r", PRNumber: 1, ReviewID: 2, ReviewerLogin: "b",
			State: "PENDING", CommitID: "c1"},
		{Org: "o", Repo: "r", PRNumber: 1, ReviewID: 3, ReviewerLogin: "c",
			State: "SOME_FUTURE_STATE", CommitID: "c1", SubmittedAt: time.Now()},
	}
	require.NoError(t, db.UpsertReviews(ctx, reviews),
		"a PENDING or unknown review state must not fail the batch")

	got, err := db.GetReviewsForPR(ctx, "o", "r", 1)
	require.NoError(t, err)
	states := map[string]bool{}
	for _, rv := range got {
		states[rv.State] = true
	}
	assert.True(t, states["APPROVED"])
	assert.True(t, states["SOME_FUTURE_STATE"], "unknown states must be stored for forward compatibility")
	assert.False(t, states["PENDING"], "draft reviews must be filtered out")
}

func TestSyncCursorLastSHARoundTrip(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	cur := model.SyncCursor{
		Org: "o", Repo: "r", Branch: "main",
		LastDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		LastSHA:  "abc123def456",
	}
	require.NoError(t, db.UpsertSyncCursor(ctx, cur))

	got, err := db.GetSyncCursor(ctx, "o", "r", "main")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "abc123def456", got.LastSHA)

	// Legacy row with NULL last_sha must read back as empty string.
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO sync_cursors (org, repo, branch, last_date, updated_at)
		VALUES ('o', 'legacy', 'main', TIMESTAMP '2024-01-01', TIMESTAMP '2024-01-01')`)
	require.NoError(t, err)
	legacy, err := db.GetSyncCursor(ctx, "o", "legacy", "main")
	require.NoError(t, err)
	require.NotNil(t, legacy)
	assert.Empty(t, legacy.LastSHA)
}

// Regression: the 72h cursor overlap re-lists already-synced commits, whose
// list/compare rows carry no diff stats. A full-row REPLACE wiped the
// stats persisted by the lazy detail fetch, and the next offline re-audit
// read 0/0 as "empty" — a false compliant waiver.
func TestUpsertCommits_PreservesVerifiedDetail(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	c := model.Commit{
		Org: "o", Repo: "r", SHA: "abc", AuthorLogin: "dev", AuthorID: 1,
		CommittedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Message:     "real change", ParentCount: 1,
	}
	require.NoError(t, db.UpsertCommits(ctx, []model.Commit{c})) // stat-less list row
	require.NoError(t, db.MarkCommitDetail(ctx, "o", "r", "abc", 42, 7, 3))

	// Overlap re-list: same commit arrives stat-less again.
	require.NoError(t, db.UpsertCommits(ctx, []model.Commit{c}))

	got, err := db.GetCommitsBySHA(ctx, "o", "r", []string{"abc"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 42, got[0].Additions, "verified stats must survive stat-less re-ingestion")
	assert.Equal(t, 7, got[0].Deletions)
	assert.Equal(t, 3, got[0].FilesChanged)
	assert.True(t, got[0].StatsVerified)

	// Verified-zero (a truly empty commit) survives too and stays
	// distinguishable from never-fetched.
	require.NoError(t, db.MarkCommitDetail(ctx, "o", "r", "abc", 0, 0, 0))
	require.NoError(t, db.UpsertCommits(ctx, []model.Commit{c}))
	got, err = db.GetCommitsBySHA(ctx, "o", "r", []string{"abc"})
	require.NoError(t, err)
	assert.True(t, got[0].StatsVerified, "verified-zero must not collapse back to never-fetched")
	assert.Zero(t, got[0].FilesChanged)
}

// LIST-shape PR responses (backfill's repo index) always omit merged_by; a
// re-upsert from that shape must not wipe the identity a detail fetch
// persisted earlier — the report's "self-merged" signal depends on it.
func TestUpsertPullRequests_PreservesMergedBy(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	rich := model.PullRequest{
		Org: "o", Repo: "r", Number: 7, Merged: true, HeadSHA: "h",
		AuthorLogin: "dev", AuthorID: 1,
		MergedByLogin: "merger", MergedByID: 99,
		MergedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, db.UpsertPullRequests(ctx, []model.PullRequest{rich}))

	listShape := rich
	listShape.MergedByLogin = ""
	listShape.MergedByID = 0
	require.NoError(t, db.UpsertPullRequests(ctx, []model.PullRequest{listShape}))

	got, err := db.GetPullRequest(ctx, "o", "r", 7)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "merger", got.MergedByLogin, "detail-only field must survive list-shape re-ingestion")
	assert.Equal(t, int64(99), got.MergedByID)
}
