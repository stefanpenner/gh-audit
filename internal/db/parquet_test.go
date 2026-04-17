package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

func seedTestData(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()

	commits := []model.Commit{
		{
			Org: "org1", Repo: "repo1", SHA: "aaa",
			AuthorLogin: "alice", AuthorEmail: "alice@example.com",
			CommittedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Message: "first", ParentCount: 1, Additions: 10, Deletions: 5,
			Href: "https://github.com/org1/repo1/commit/aaa",
		},
		{
			Org: "org1", Repo: "repo1", SHA: "bbb",
			AuthorLogin: "bob", AuthorEmail: "bob@example.com",
			CommittedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
			Message: "second", ParentCount: 1, Additions: 3, Deletions: 1,
			Href: "https://github.com/org1/repo1/commit/bbb",
		},
	}
	if err := db.UpsertCommits(ctx, commits); err != nil {
		t.Fatalf("UpsertCommits: %v", err)
	}

	prs := []model.PullRequest{
		{
			Org: "org1", Repo: "repo1", Number: 42,
			Title: "Add feature", Merged: true,
			HeadSHA: "aaa", MergeCommitSHA: "bbb",
			AuthorLogin: "alice",
			MergedAt:    time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC),
			Href:        "https://github.com/org1/repo1/pull/42",
		},
	}
	if err := db.UpsertPullRequests(ctx, prs); err != nil {
		t.Fatalf("UpsertPullRequests: %v", err)
	}

	if err := db.UpsertCommitPRs(ctx, "org1", "repo1", "bbb", []int{42}); err != nil {
		t.Fatalf("UpsertCommitPRs: %v", err)
	}

	if err := db.UpsertCommitBranches(ctx, "org1", "repo1", []string{"aaa", "bbb"}, "main"); err != nil {
		t.Fatalf("UpsertCommitBranches: %v", err)
	}

	reviews := []model.Review{
		{
			Org: "org1", Repo: "repo1", PRNumber: 42,
			ReviewID: 100, ReviewerLogin: "bob",
			State: "APPROVED", CommitID: "aaa",
			SubmittedAt: time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC),
			Href:        "https://github.com/org1/repo1/pull/42#pullrequestreview-100",
		},
	}
	if err := db.UpsertReviews(ctx, reviews); err != nil {
		t.Fatalf("UpsertReviews: %v", err)
	}

	checkRuns := []model.CheckRun{
		{
			Org: "org1", Repo: "repo1", CommitSHA: "aaa",
			CheckRunID: 200, CheckName: "ci",
			Status: "completed", Conclusion: "success",
			CompletedAt: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC),
		},
	}
	if err := db.UpsertCheckRuns(ctx, checkRuns); err != nil {
		t.Fatalf("UpsertCheckRuns: %v", err)
	}

	auditResults := []model.AuditResult{
		{
			Org: "org1", Repo: "repo1", SHA: "aaa",
			IsCompliant:        true,
			HasPR:              true,
			PRNumber:           42,
			HasFinalApproval:   true,
			ApproverLogins:     []string{"bob"},
			OwnerApprovalCheck: "success",
			CommitHref:         "https://github.com/org1/repo1/commit/aaa",
			PRHref:             "https://github.com/org1/repo1/pull/42",
		},
	}
	if err := db.UpsertAuditResults(ctx, auditResults); err != nil {
		t.Fatalf("UpsertAuditResults: %v", err)
	}

	cursor := model.SyncCursor{
		Org: "org1", Repo: "repo1", Branch: "main",
		LastDate: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	if err := db.UpsertSyncCursor(ctx, cursor); err != nil {
		t.Fatalf("UpsertSyncCursor: %v", err)
	}
}

func TestParquetRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcDB := mustOpenMemory(t)

	seedTestData(t, srcDB, ctx)

	// Export to temp dir
	dir := t.TempDir()
	if err := srcDB.ExportParquet(ctx, dir); err != nil {
		t.Fatalf("ExportParquet: %v", err)
	}

	// Verify parquet files were created
	for _, table := range parquetTables {
		path := filepath.Join(dir, table+".parquet")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected parquet file %s to exist: %v", path, err)
		}
	}

	// Import into fresh DB
	dstDB := mustOpenMemory(t)
	if err := dstDB.ImportParquet(ctx, dir); err != nil {
		t.Fatalf("ImportParquet: %v", err)
	}

	// Verify commits
	commits, err := dstDB.GetUnauditedCommits(ctx, "org1", "repo1")
	if err != nil {
		t.Fatalf("GetUnauditedCommits: %v", err)
	}
	// bbb has an audit result but aaa also has one, so both are audited.
	// Use GetCommitsBySHA instead.
	allCommits, err := dstDB.GetCommitsBySHA(ctx, "org1", "repo1", []string{"aaa", "bbb"})
	if err != nil {
		t.Fatalf("GetCommitsBySHA: %v", err)
	}
	if len(allCommits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(allCommits))
	}
	_ = commits

	// Verify PRs
	prs, err := dstDB.GetPRsForCommit(ctx, "org1", "repo1", "bbb")
	if err != nil {
		t.Fatalf("GetPRsForCommit: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 42 {
		t.Fatalf("expected PR #42, got %v", prs)
	}

	// Verify reviews
	reviews, err := dstDB.GetReviewsForPR(ctx, "org1", "repo1", 42)
	if err != nil {
		t.Fatalf("GetReviewsForPR: %v", err)
	}
	if len(reviews) != 1 || reviews[0].State != "APPROVED" {
		t.Fatalf("expected 1 APPROVED review, got %v", reviews)
	}

	// Verify check runs
	checkRuns, err := dstDB.GetCheckRunsForCommit(ctx, "org1", "repo1", "aaa")
	if err != nil {
		t.Fatalf("GetCheckRunsForCommit: %v", err)
	}
	if len(checkRuns) != 1 || checkRuns[0].Conclusion != "success" {
		t.Fatalf("expected 1 success check run, got %v", checkRuns)
	}

	// Verify sync cursor
	cursor, err := dstDB.GetSyncCursor(ctx, "org1", "repo1", "main")
	if err != nil {
		t.Fatalf("GetSyncCursor: %v", err)
	}
	if cursor == nil {
		t.Fatal("expected non-nil sync cursor")
	}
	if !cursor.LastDate.Equal(time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expected LastDate 2025-01-02, got %v", cursor.LastDate)
	}
}

func TestParquetImportMergesData(t *testing.T) {
	ctx := context.Background()

	// Create source DB with original data
	srcDB := mustOpenMemory(t)
	commits := []model.Commit{
		{
			Org: "org1", Repo: "repo1", SHA: "aaa",
			AuthorLogin: "alice", AuthorEmail: "alice@example.com",
			CommittedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Message:     "original",
		},
	}
	if err := srcDB.UpsertCommits(ctx, commits); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := srcDB.ExportParquet(ctx, dir); err != nil {
		t.Fatalf("ExportParquet: %v", err)
	}

	// Create destination DB with conflicting data
	dstDB := mustOpenMemory(t)
	existing := []model.Commit{
		{
			Org: "org1", Repo: "repo1", SHA: "aaa",
			AuthorLogin: "alice", AuthorEmail: "alice@example.com",
			CommittedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Message:     "existing",
		},
		{
			Org: "org1", Repo: "repo1", SHA: "ccc",
			AuthorLogin: "carol",
			CommittedAt: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC),
			Message:     "extra",
		},
	}
	if err := dstDB.UpsertCommits(ctx, existing); err != nil {
		t.Fatal(err)
	}

	// Import should merge: replace aaa, keep ccc
	if err := dstDB.ImportParquet(ctx, dir); err != nil {
		t.Fatalf("ImportParquet: %v", err)
	}

	got, err := dstDB.GetCommitsBySHA(ctx, "org1", "repo1", []string{"aaa"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(got))
	}
	if got[0].Message != "original" {
		t.Errorf("expected message 'original' (from import), got %q", got[0].Message)
	}

	// ccc should still exist
	got2, err := dstDB.GetCommitsBySHA(ctx, "org1", "repo1", []string{"ccc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 {
		t.Fatalf("expected commit ccc to still exist, got %d", len(got2))
	}
}

func TestParquetImportSkipsMissingFiles(t *testing.T) {
	ctx := context.Background()
	db := mustOpenMemory(t)

	// Import from empty directory - should succeed with no errors
	dir := t.TempDir()
	if err := db.ImportParquet(ctx, dir); err != nil {
		t.Fatalf("ImportParquet with no files should succeed, got: %v", err)
	}

	// Import from directory with only some files
	srcDB := mustOpenMemory(t)
	commits := []model.Commit{
		{
			Org: "org1", Repo: "repo1", SHA: "aaa",
			AuthorLogin: "alice",
			CommittedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Message:     "test",
		},
	}
	if err := srcDB.UpsertCommits(ctx, commits); err != nil {
		t.Fatal(err)
	}

	dir2 := t.TempDir()
	if err := srcDB.ExportParquet(ctx, dir2); err != nil {
		t.Fatal(err)
	}

	// Remove all but commits.parquet
	for _, table := range parquetTables {
		if table == "commits" {
			continue
		}
		os.Remove(filepath.Join(dir2, table+".parquet"))
	}

	// Should import only commits without error
	if err := db.ImportParquet(ctx, dir2); err != nil {
		t.Fatalf("ImportParquet with partial files should succeed, got: %v", err)
	}

	got, err := db.GetCommitsBySHA(ctx, "org1", "repo1", []string{"aaa"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 commit after partial import, got %d", len(got))
	}
}
