package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

func mustOpenMemory(t *testing.T) *DB {
	t.Helper()
	db, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSchemaMigration(t *testing.T) {
	// Simply opening an in-memory DB runs migrate; no error means success.
	_ = mustOpenMemory(t)
}

func TestUpsertCommitsAndGetUnaudited(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

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

	got, err := db.GetUnauditedCommits(ctx, "org1", "repo1")
	if err != nil {
		t.Fatalf("GetUnauditedCommits: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 unaudited commits, got %d", len(got))
	}
}

func TestUpsertCommitsIdempotent(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	c := model.Commit{
		Org: "org1", Repo: "repo1", SHA: "aaa",
		AuthorLogin: "alice", AuthorEmail: "alice@example.com",
		CommittedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Message: "first", ParentCount: 1, Additions: 10, Deletions: 5,
	}

	if err := db.UpsertCommits(ctx, []model.Commit{c}); err != nil {
		t.Fatalf("first UpsertCommits: %v", err)
	}

	// Update message and re-upsert
	c.Message = "updated"
	if err := db.UpsertCommits(ctx, []model.Commit{c}); err != nil {
		t.Fatalf("second UpsertCommits: %v", err)
	}

	got, err := db.GetCommitsBySHA(ctx, "org1", "repo1", []string{"aaa"})
	if err != nil {
		t.Fatalf("GetCommitsBySHA: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(got))
	}
	if got[0].Message != "updated" {
		t.Fatalf("expected message 'updated', got %q", got[0].Message)
	}
}

func TestBatchInsertOver500(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	commits := make([]model.Commit, 501)
	for i := range commits {
		commits[i] = model.Commit{
			Org: "org1", Repo: "repo1", SHA: fmt.Sprintf("sha%04d", i),
			AuthorLogin: "user", CommittedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Message: fmt.Sprintf("commit %d", i),
		}
	}

	if err := db.UpsertCommits(ctx, commits); err != nil {
		t.Fatalf("UpsertCommits with >500: %v", err)
	}

	got, err := db.GetUnauditedCommits(ctx, "org1", "repo1")
	if err != nil {
		t.Fatalf("GetUnauditedCommits: %v", err)
	}
	if len(got) != 501 {
		t.Fatalf("expected 501 commits, got %d", len(got))
	}
}

func TestUpsertPullRequestsAndGetPRsForCommit(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	prs := []model.PullRequest{
		{
			Org: "org1", Repo: "repo1", Number: 42,
			Title: "Add feature", Merged: true,
			HeadSHA: "abc", MergeCommitSHA: "def",
			AuthorLogin: "alice",
			MergedAt: time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC),
			Href: "https://github.com/org1/repo1/pull/42",
		},
	}

	if err := db.UpsertPullRequests(ctx, prs); err != nil {
		t.Fatalf("UpsertPullRequests: %v", err)
	}

	if err := db.UpsertCommitPRs(ctx, "org1", "repo1", "def", []int{42}); err != nil {
		t.Fatalf("UpsertCommitPRs: %v", err)
	}

	got, err := db.GetPRsForCommit(ctx, "org1", "repo1", "def")
	if err != nil {
		t.Fatalf("GetPRsForCommit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(got))
	}
	if got[0].Number != 42 {
		t.Fatalf("expected PR #42, got #%d", got[0].Number)
	}
}

func TestUpsertReviewsAndGetReviewsForPR(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	reviews := []model.Review{
		{
			Org: "org1", Repo: "repo1", PRNumber: 42,
			ReviewID: 100, ReviewerLogin: "bob",
			State: "APPROVED", CommitID: "abc",
			SubmittedAt: time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC),
			Href: "https://github.com/org1/repo1/pull/42#pullrequestreview-100",
		},
	}

	if err := db.UpsertReviews(ctx, reviews); err != nil {
		t.Fatalf("UpsertReviews: %v", err)
	}

	got, err := db.GetReviewsForPR(ctx, "org1", "repo1", 42)
	if err != nil {
		t.Fatalf("GetReviewsForPR: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 review, got %d", len(got))
	}
	if got[0].State != "APPROVED" {
		t.Fatalf("expected APPROVED, got %q", got[0].State)
	}
}

func TestUpsertCheckRunsAndGetCheckRunsForCommit(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	checkRuns := []model.CheckRun{
		{
			Org: "org1", Repo: "repo1", CommitSHA: "abc",
			CheckRunID: 200, CheckName: "ci",
			Status: "completed", Conclusion: "success",
			CompletedAt: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC),
		},
	}

	if err := db.UpsertCheckRuns(ctx, checkRuns); err != nil {
		t.Fatalf("UpsertCheckRuns: %v", err)
	}

	got, err := db.GetCheckRunsForCommit(ctx, "org1", "repo1", "abc")
	if err != nil {
		t.Fatalf("GetCheckRunsForCommit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 check run, got %d", len(got))
	}
	if got[0].Conclusion != "success" {
		t.Fatalf("expected conclusion 'success', got %q", got[0].Conclusion)
	}
}

func TestCommitPRsLink(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	// Insert two PRs
	prs := []model.PullRequest{
		{Org: "org1", Repo: "repo1", Number: 10, Title: "PR10", AuthorLogin: "alice",
			MergedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Org: "org1", Repo: "repo1", Number: 20, Title: "PR20", AuthorLogin: "bob",
			MergedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)},
	}
	if err := db.UpsertPullRequests(ctx, prs); err != nil {
		t.Fatal(err)
	}

	// Link both to same commit
	if err := db.UpsertCommitPRs(ctx, "org1", "repo1", "sha1", []int{10, 20}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetPRsForCommit(ctx, "org1", "repo1", "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 PRs linked, got %d", len(got))
	}
}

func TestUpsertAuditResultsAndGetAuditResults(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	// Insert commit first (for join)
	commits := []model.Commit{
		{
			Org: "org1", Repo: "repo1", SHA: "sha1",
			AuthorLogin: "alice",
			CommittedAt: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC),
			Message: "feat: something",
		},
	}
	if err := db.UpsertCommits(ctx, commits); err != nil {
		t.Fatal(err)
	}

	results := []model.AuditResult{
		{
			Org: "org1", Repo: "repo1", SHA: "sha1",
			IsEmptyCommit: false, IsBot: false, HasPR: true, PRNumber: 42,
			HasFinalApproval: true,
			ApproverLogins:   []string{"bob", "carol"},
			OwnerApprovalCheck: "success",
			IsCompliant: true,
			Reasons:     nil,
			CommitHref:  "https://github.com/org1/repo1/commit/sha1",
			PRHref:      "https://github.com/org1/repo1/pull/42",
		},
	}

	if err := db.UpsertAuditResults(ctx, results); err != nil {
		t.Fatalf("UpsertAuditResults: %v", err)
	}

	got, err := db.GetAuditResults(ctx, AuditQueryOpts{Org: "org1", Repo: "repo1"})
	if err != nil {
		t.Fatalf("GetAuditResults: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(got))
	}
	if got[0].AuthorLogin != "alice" {
		t.Errorf("expected author 'alice', got %q", got[0].AuthorLogin)
	}
	if !got[0].IsCompliant {
		t.Error("expected IsCompliant=true")
	}
	if len(got[0].ApproverLogins) != 2 {
		t.Errorf("expected 2 approvers, got %d", len(got[0].ApproverLogins))
	}

	// Verify the commit is now audited
	unaudited, err := db.GetUnauditedCommits(ctx, "org1", "repo1")
	if err != nil {
		t.Fatal(err)
	}
	if len(unaudited) != 0 {
		t.Errorf("expected 0 unaudited commits, got %d", len(unaudited))
	}
}

func TestGetAuditResultsFilters(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	// Insert two commits: one compliant, one not
	commits := []model.Commit{
		{Org: "org1", Repo: "repo1", SHA: "good", AuthorLogin: "alice",
			CommittedAt: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), Message: "ok"},
		{Org: "org1", Repo: "repo1", SHA: "bad", AuthorLogin: "bob",
			CommittedAt: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC), Message: "not ok"},
		{Org: "org2", Repo: "repo2", SHA: "other", AuthorLogin: "carol",
			CommittedAt: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC), Message: "other org"},
	}
	if err := db.UpsertCommits(ctx, commits); err != nil {
		t.Fatal(err)
	}

	auditResults := []model.AuditResult{
		{Org: "org1", Repo: "repo1", SHA: "good", IsCompliant: true},
		{Org: "org1", Repo: "repo1", SHA: "bad", IsCompliant: false,
			Reasons: []string{"no PR", "no approval"}},
		{Org: "org2", Repo: "repo2", SHA: "other", IsCompliant: true},
	}
	if err := db.UpsertAuditResults(ctx, auditResults); err != nil {
		t.Fatal(err)
	}

	t.Run("filter by org", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{Org: "org1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2, got %d", len(got))
		}
	})

	t.Run("filter by repo", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{Org: "org1", Repo: "repo1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2, got %d", len(got))
		}
	})

	t.Run("filter since", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{
			Since: time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 (april + may), got %d", len(got))
		}
	})

	t.Run("filter until", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{
			Until: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 (march only), got %d", len(got))
		}
	})

	t.Run("only failures", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{OnlyFailures: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 failure, got %d", len(got))
		}
		if got[0].SHA != "bad" {
			t.Errorf("expected sha 'bad', got %q", got[0].SHA)
		}
		if len(got[0].Reasons) != 2 {
			t.Errorf("expected 2 reasons, got %d", len(got[0].Reasons))
		}
	})
}

func TestSyncCursorRoundTrip(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	cursor := model.SyncCursor{
		Org:      "org1",
		Repo:     "repo1",
		Branch:   "main",
		LastDate: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC),
	}

	if err := db.UpsertSyncCursor(ctx, cursor); err != nil {
		t.Fatalf("UpsertSyncCursor: %v", err)
	}

	got, err := db.GetSyncCursor(ctx, "org1", "repo1", "main")
	if err != nil {
		t.Fatalf("GetSyncCursor: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil cursor")
	}
	if got.Org != "org1" || got.Repo != "repo1" || got.Branch != "main" {
		t.Errorf("unexpected cursor: %+v", got)
	}
	if !got.LastDate.Equal(cursor.LastDate) {
		t.Errorf("expected LastDate %v, got %v", cursor.LastDate, got.LastDate)
	}
}

func TestSyncCursorPerBranch(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	mainCursor := model.SyncCursor{
		Org: "org1", Repo: "repo1", Branch: "main",
		LastDate: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	releaseCursor := model.SyncCursor{
		Org: "org1", Repo: "repo1", Branch: "release/1.0",
		LastDate: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
	}

	if err := db.UpsertSyncCursor(ctx, mainCursor); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSyncCursor(ctx, releaseCursor); err != nil {
		t.Fatal(err)
	}

	gotMain, err := db.GetSyncCursor(ctx, "org1", "repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if gotMain == nil || !gotMain.LastDate.Equal(mainCursor.LastDate) {
		t.Errorf("main cursor mismatch: %+v", gotMain)
	}

	gotRelease, err := db.GetSyncCursor(ctx, "org1", "repo1", "release/1.0")
	if err != nil {
		t.Fatal(err)
	}
	if gotRelease == nil || !gotRelease.LastDate.Equal(releaseCursor.LastDate) {
		t.Errorf("release cursor mismatch: %+v", gotRelease)
	}
}

func TestSyncCursorNotFound(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	got, err := db.GetSyncCursor(ctx, "nonexistent", "nope", "main")
	if err != nil {
		t.Fatalf("GetSyncCursor: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil cursor, got %+v", got)
	}
}

func TestUpsertCommitBranches(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	err := db.UpsertCommitBranches(ctx, "org1", "repo1", []string{"sha1", "sha2"}, "main")
	if err != nil {
		t.Fatalf("UpsertCommitBranches: %v", err)
	}

	// Upsert same commits to another branch
	err = db.UpsertCommitBranches(ctx, "org1", "repo1", []string{"sha1"}, "release/1.0")
	if err != nil {
		t.Fatalf("UpsertCommitBranches: %v", err)
	}

	// Idempotent: upsert again should not fail
	err = db.UpsertCommitBranches(ctx, "org1", "repo1", []string{"sha1"}, "main")
	if err != nil {
		t.Fatalf("UpsertCommitBranches idempotent: %v", err)
	}
}

func TestEmptyCommitStoredCorrectly(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	commits := []model.Commit{
		{
			Org: "org1", Repo: "repo1", SHA: "empty",
			AuthorLogin: "bot",
			CommittedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Message: "empty commit", Additions: 0, Deletions: 0,
		},
	}

	if err := db.UpsertCommits(ctx, commits); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetCommitsBySHA(ctx, "org1", "repo1", []string{"empty"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].Additions != 0 || got[0].Deletions != 0 {
		t.Errorf("expected 0/0 additions/deletions, got %d/%d", got[0].Additions, got[0].Deletions)
	}
}
