package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustOpenMemory(t *testing.T) *DB {
	t.Helper()
	db, err := OpenMemory()
	require.NoError(t, err, "OpenMemory")
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

	require.NoError(t, db.UpsertCommits(ctx, commits))

	got, err := db.GetUnauditedCommits(ctx, "org1", "repo1")
	require.NoError(t, err)
	require.Len(t, got, 2)
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

	require.NoError(t, db.UpsertCommits(ctx, []model.Commit{c}))

	// Update message and re-upsert
	c.Message = "updated"
	require.NoError(t, db.UpsertCommits(ctx, []model.Commit{c}))

	got, err := db.GetCommitsBySHA(ctx, "org1", "repo1", []string{"aaa"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "updated", got[0].Message)
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

	require.NoError(t, db.UpsertCommits(ctx, commits))

	got, err := db.GetUnauditedCommits(ctx, "org1", "repo1")
	require.NoError(t, err)
	require.Len(t, got, 501)
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

	require.NoError(t, db.UpsertPullRequests(ctx, prs))
	require.NoError(t, db.UpsertCommitPRs(ctx, "org1", "repo1", "def", []int{42}))

	got, err := db.GetPRsForCommit(ctx, "org1", "repo1", "def")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 42, got[0].Number)
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

	require.NoError(t, db.UpsertReviews(ctx, reviews))

	got, err := db.GetReviewsForPR(ctx, "org1", "repo1", 42)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "APPROVED", got[0].State)
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

	require.NoError(t, db.UpsertCheckRuns(ctx, checkRuns))

	got, err := db.GetCheckRunsForCommit(ctx, "org1", "repo1", "abc")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "success", got[0].Conclusion)
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
	require.NoError(t, db.UpsertPullRequests(ctx, prs))

	// Link both to same commit
	require.NoError(t, db.UpsertCommitPRs(ctx, "org1", "repo1", "sha1", []int{10, 20}))

	got, err := db.GetPRsForCommit(ctx, "org1", "repo1", "sha1")
	require.NoError(t, err)
	require.Len(t, got, 2)
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
	require.NoError(t, db.UpsertCommits(ctx, commits))

	results := []model.AuditResult{
		{
			Org: "org1", Repo: "repo1", SHA: "sha1",
			IsEmptyCommit: false, IsBot: false, IsExemptAuthor: false, HasPR: true, PRNumber: 42,
			HasFinalApproval: true,
			ApproverLogins:   []string{"bob", "carol"},
			OwnerApprovalCheck: "success",
			IsCompliant: true,
			Reasons:     nil,
			CommitHref:  "https://github.com/org1/repo1/commit/sha1",
			PRHref:      "https://github.com/org1/repo1/pull/42",
		},
	}

	require.NoError(t, db.UpsertAuditResults(ctx, results))

	got, err := db.GetAuditResults(ctx, AuditQueryOpts{Org: "org1", Repo: "repo1"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "alice", got[0].AuthorLogin)
	assert.True(t, got[0].IsCompliant)
	assert.Len(t, got[0].ApproverLogins, 2)

	// Verify the commit is now audited
	unaudited, err := db.GetUnauditedCommits(ctx, "org1", "repo1")
	require.NoError(t, err)
	assert.Empty(t, unaudited)
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
	require.NoError(t, db.UpsertCommits(ctx, commits))

	auditResults := []model.AuditResult{
		{Org: "org1", Repo: "repo1", SHA: "good", IsCompliant: true},
		{Org: "org1", Repo: "repo1", SHA: "bad", IsCompliant: false,
			Reasons: []string{"no PR", "no approval"}},
		{Org: "org2", Repo: "repo2", SHA: "other", IsCompliant: true},
	}
	require.NoError(t, db.UpsertAuditResults(ctx, auditResults))

	t.Run("filter by org", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{Org: "org1"})
		require.NoError(t, err)
		require.Len(t, got, 2)
	})

	t.Run("filter by repo", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{Org: "org1", Repo: "repo1"})
		require.NoError(t, err)
		require.Len(t, got, 2)
	})

	t.Run("filter since", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{
			Since: time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		require.Len(t, got, 2)
	})

	t.Run("filter until", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{
			Until: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("only failures", func(t *testing.T) {
		got, err := db.GetAuditResults(ctx, AuditQueryOpts{OnlyFailures: true})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "bad", got[0].SHA)
		assert.Len(t, got[0].Reasons, 2)
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

	require.NoError(t, db.UpsertSyncCursor(ctx, cursor))

	got, err := db.GetSyncCursor(ctx, "org1", "repo1", "main")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "org1", got.Org)
	assert.Equal(t, "repo1", got.Repo)
	assert.Equal(t, "main", got.Branch)
	assert.True(t, got.LastDate.Equal(cursor.LastDate))
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

	require.NoError(t, db.UpsertSyncCursor(ctx, mainCursor))
	require.NoError(t, db.UpsertSyncCursor(ctx, releaseCursor))

	gotMain, err := db.GetSyncCursor(ctx, "org1", "repo1", "main")
	require.NoError(t, err)
	require.NotNil(t, gotMain)
	assert.True(t, gotMain.LastDate.Equal(mainCursor.LastDate))

	gotRelease, err := db.GetSyncCursor(ctx, "org1", "repo1", "release/1.0")
	require.NoError(t, err)
	require.NotNil(t, gotRelease)
	assert.True(t, gotRelease.LastDate.Equal(releaseCursor.LastDate))
}

func TestSyncCursorNotFound(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	got, err := db.GetSyncCursor(ctx, "nonexistent", "nope", "main")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestUpsertCommitBranches(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	err := db.UpsertCommitBranches(ctx, "org1", "repo1", []string{"sha1", "sha2"}, "main")
	require.NoError(t, err)

	// Upsert same commits to another branch
	err = db.UpsertCommitBranches(ctx, "org1", "repo1", []string{"sha1"}, "release/1.0")
	require.NoError(t, err)

	// Idempotent: upsert again should not fail
	err = db.UpsertCommitBranches(ctx, "org1", "repo1", []string{"sha1"}, "main")
	require.NoError(t, err)
}

func TestEnumColumnsAcceptValidValues(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	// Insert a review with a valid enum state and read it back.
	reviews := []model.Review{
		{
			Org: "org1", Repo: "repo1", PRNumber: 1,
			ReviewID: 1, ReviewerLogin: "alice",
			State: "APPROVED", CommitID: "abc",
			SubmittedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	require.NoError(t, db.UpsertReviews(ctx, reviews))

	got, err := db.GetReviewsForPR(ctx, "org1", "repo1", 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "APPROVED", got[0].State)

	// Inserting an invalid enum value should fail.
	_, err = db.ExecContext(ctx,
		`INSERT INTO reviews (org, repo, pr_number, review_id, state)
		 VALUES ('org1', 'repo1', 2, 2, 'INVALID_STATE')`)
	require.Error(t, err)

	// Verify check_runs accept arbitrary status/conclusion values (TEXT columns).
	checkRuns := []model.CheckRun{
		{
			Org: "org1", Repo: "repo1", CommitSHA: "abc",
			CheckRunID: 1, CheckName: "ci",
			Status: "completed", Conclusion: "success",
			CompletedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			Org: "org1", Repo: "repo1", CommitSHA: "abc",
			CheckRunID: 2, CheckName: "ci2",
			Status: "waiting", Conclusion: "startup_failure",
			CompletedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	require.NoError(t, db.UpsertCheckRuns(ctx, checkRuns))
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

	require.NoError(t, db.UpsertCommits(ctx, commits))

	got, err := db.GetCommitsBySHA(ctx, "org1", "repo1", []string{"empty"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 0, got[0].Additions)
	assert.Equal(t, 0, got[0].Deletions)
}
