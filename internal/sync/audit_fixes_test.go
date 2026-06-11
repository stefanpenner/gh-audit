package sync

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// statsBySHA builds a StatsFetcher serving canned (additions, deletions,
// filesChanged) triples. SHAs not in the map return an error.
func statsBySHA(stats map[string][3]int) StatsFetcher {
	return func(_ StatsTrigger, _, _, sha string) (int, int, int, error) {
		if s, ok := stats[sha]; ok {
			return s[0], s[1], s[2], nil
		}
		return 0, 0, 0, errors.New("no stats available")
	}
}

func exemptBotAuthors() []model.ExemptAuthor {
	return []model.ExemptAuthor{{Login: "ci-bot", ID: 99}}
}

// botSquashWithHumanBranchCommit models the §1 fail-open scenario: an
// exempt CI bot squash-merges a PR whose branch contains a human commit.
// PR-branch commits arrive from /pulls/{n}/commits with NO diff stats —
// production rows are always 0/0 here.
func botSquashWithHumanBranchCommit() (model.Commit, model.EnrichmentResult) {
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "squash-sha",
		AuthorID: 99, AuthorLogin: "ci-bot",
		Additions: 50, Deletions: 5, ParentCount: 1,
		CommittedAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
	}
	enrichment := model.EnrichmentResult{
		Commit: commit,
		PRs: []model.PullRequest{{
			Org: "o", Repo: "r", Number: 7, Merged: true, HeadSHA: "head-sha",
			AuthorID: 99, MergedAt: commit.CommittedAt,
		}},
		PRBranchCommits: map[int][]model.Commit{
			7: {
				{Org: "o", Repo: "r", SHA: "human-sha", AuthorID: 7, AuthorLogin: "human",
					// No stats — /pulls/{n}/commits omits them.
					Additions: 0, Deletions: 0},
			},
		},
	}
	return commit, enrichment
}

// The §1 carve-out must not be granted on the strength of locally-zero
// stats: every PR-branch commit looks 0/0 in production, so skipping
// "empty" commits unverified skips ALL of them and waives unreviewed
// human code.
func TestExemptAuthor_UnverifiableHumanCommitFailsClosed(t *testing.T) {
	commit, enrichment := botSquashWithHumanBranchCommit()

	t.Run("nil fetcher fails closed", func(t *testing.T) {
		result := EvaluateCommit(commit, enrichment, exemptBotAuthors(), nil, nil)
		assert.True(t, result.IsExemptAuthor, "author match must still be flagged for visibility")
		assert.False(t, result.IsCompliant, "exemption must not waive a squash with an unverifiable human commit")
	})

	t.Run("fetch error fails closed", func(t *testing.T) {
		fetcher := statsBySHA(map[string][3]int{}) // every fetch errors
		result := EvaluateCommit(commit, enrichment, exemptBotAuthors(), nil, fetcher)
		assert.False(t, result.IsCompliant)
	})

	t.Run("verified non-empty human commit fails closed", func(t *testing.T) {
		fetcher := statsBySHA(map[string][3]int{"human-sha": {12, 3, 2}})
		result := EvaluateCommit(commit, enrichment, exemptBotAuthors(), nil, fetcher)
		assert.False(t, result.IsCompliant)
	})

	t.Run("verified empty human commit keeps the carve-out", func(t *testing.T) {
		fetcher := statsBySHA(map[string][3]int{"human-sha": {0, 0, 0}})
		result := EvaluateCommit(commit, enrichment, exemptBotAuthors(), nil, fetcher)
		assert.True(t, result.IsCompliant, "a rerun-check empty commit must not void the exemption")
		assert.Equal(t, []string{"exempt: configured author"}, result.Reasons)
	})
}

// A transient stats-fetch error must not mint a permanent compliant
// "empty commit" row — fail closed to non-compliant instead.
func TestEmptyCommitFallback_FetchErrorDoesNotWaive(t *testing.T) {
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "direct-push",
		AuthorID: 7, AuthorLogin: "human",
		Additions: 0, Deletions: 0, ParentCount: 1, // stats unknown locally
	}
	failingFetcher := statsBySHA(map[string][3]int{}) // errors for every SHA

	result := EvaluateCommit(commit, model.EnrichmentResult{Commit: commit}, nil, nil, failingFetcher)

	assert.False(t, result.IsCompliant)
	assert.False(t, result.IsEmptyCommit)
	assert.Equal(t, []string{"no associated pull request"}, result.Reasons)
}

func TestEmptyCommitFallback_VerifiedEmptyStillWaives(t *testing.T) {
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "rerun-check",
		AuthorID: 7, AuthorLogin: "human",
		Additions: 0, Deletions: 0, ParentCount: 1,
	}
	fetcher := statsBySHA(map[string][3]int{"rerun-check": {0, 0, 0}})

	result := EvaluateCommit(commit, model.EnrichmentResult{Commit: commit}, nil, nil, fetcher)

	assert.True(t, result.IsCompliant)
	assert.True(t, result.IsEmptyCommit)
}

// §8 must apply on the no-PR path: a diff-verified clean revert pushed
// directly (no PR) is waived, matching Architecture.md's "§8 runs on any
// non-compliant primary verdict" and the report layer's expectations.
func TestCleanRevert_WaivedOnDirectPush(t *testing.T) {
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "revert-sha",
		AuthorID: 7, AuthorLogin: "human",
		Additions: 10, Deletions: 10, ParentCount: 1,
		Message: `Revert "bad change"`,
	}
	enrichment := model.EnrichmentResult{
		Commit:             commit,
		IsCleanRevert:      true,
		RevertVerification: "diff-verified",
		RevertedSHA:        "abcdef1234567890",
	}

	result := EvaluateCommit(commit, enrichment, nil, nil, nil)

	assert.True(t, result.IsCompliant)
	assert.False(t, result.HasPR)
	assert.True(t, result.IsCleanRevert)
	require.Len(t, result.Reasons, 1)
	assert.Contains(t, result.Reasons[0], "clean revert of abcdef123456")
}

// Re-runs mint duplicate same-named check runs; only the latest run (by
// CompletedAt, CheckRunID tiebreak) may decide the §6 verdict.
func TestRequiredChecks_LatestRunWins(t *testing.T) {
	required := []RequiredCheck{{Name: "Owner Approval", Conclusion: "success"}}
	t1 := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Minute)

	failedThenPassed := []model.CheckRun{
		{CommitSHA: "head", CheckName: "Owner Approval", Conclusion: "failure", CheckRunID: 1, CompletedAt: t1},
		{CommitSHA: "head", CheckName: "Owner Approval", Conclusion: "success", CheckRunID: 2, CompletedAt: t2},
	}
	passedThenFailed := []model.CheckRun{
		{CommitSHA: "head", CheckName: "Owner Approval", Conclusion: "success", CheckRunID: 1, CompletedAt: t1},
		{CommitSHA: "head", CheckName: "Owner Approval", Conclusion: "failure", CheckRunID: 2, CompletedAt: t2},
	}

	// Row order must not matter.
	for _, runs := range [][]model.CheckRun{failedThenPassed, {failedThenPassed[1], failedThenPassed[0]}} {
		assert.Equal(t, "success", evaluateRequiredChecks(runs, "head", required))
	}
	for _, runs := range [][]model.CheckRun{passedThenFailed, {passedThenFailed[1], passedThenFailed[0]}} {
		assert.Equal(t, "failure", evaluateRequiredChecks(runs, "head", required))
	}
}

// An APPROVED and a COMMENTED review from the same reviewer at the same
// second (GitHub timestamps are second-precision) must resolve to
// APPROVED regardless of row order.
func TestLatestReviewStates_ApprovedCommentedTieOrderInsensitive(t *testing.T) {
	at := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	pr := model.PullRequest{Number: 1, HeadSHA: "head", MergedAt: at.Add(time.Hour)}
	approved := model.Review{PRNumber: 1, CommitID: "head", ReviewerID: 5, ReviewID: 100, State: "APPROVED", SubmittedAt: at}
	commented := model.Review{PRNumber: 1, CommitID: "head", ReviewerID: 5, ReviewID: 200, State: "COMMENTED", SubmittedAt: at}

	for name, reviews := range map[string][]model.Review{
		"approved first":  {approved, commented},
		"commented first": {commented, approved},
	} {
		latest, _ := latestReviewStatesOnFinal(reviews, pr)
		require.Len(t, latest, 1, name)
		for _, r := range latest {
			assert.Equal(t, "APPROVED", r.State, name)
		}
	}
}

// GitHub reports 0/0 line stats for pure renames and mode-only changes —
// but those commits move content without review and must NOT be waived as
// "empty". Emptiness is zero lines AND zero files.
func TestEmptyCommitFallback_RenameOnlyNotWaived(t *testing.T) {
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "rename-only",
		AuthorID: 7, AuthorLogin: "human",
		Additions: 0, Deletions: 0, ParentCount: 1,
	}
	fetcher := statsBySHA(map[string][3]int{"rename-only": {0, 0, 2}}) // 0/0 lines, 2 files touched

	result := EvaluateCommit(commit, model.EnrichmentResult{Commit: commit}, nil, nil, fetcher)

	assert.False(t, result.IsCompliant, "a rename-only commit moves content without review")
	assert.False(t, result.IsEmptyCommit)
}

// Offline re-audit of a row whose detail was verified at sync time must
// agree with the sync verdict: a verified rename-only row stays blocked,
// and a verified-empty human commit keeps the §1 carve-out.
func TestOfflineReaudit_UsesVerifiedDetail(t *testing.T) {
	t.Run("verified rename-only row not waived offline", func(t *testing.T) {
		commit := model.Commit{
			Org: "o", Repo: "r", SHA: "rename-only",
			AuthorID: 7, Additions: 0, Deletions: 0,
			FilesChanged: 2, StatsVerified: true, ParentCount: 1,
		}
		result := EvaluateCommit(commit, model.EnrichmentResult{Commit: commit}, nil, nil, nil)
		assert.False(t, result.IsCompliant)
	})

	t.Run("verified-empty human branch commit keeps carve-out offline", func(t *testing.T) {
		commit, enrichment := botSquashWithHumanBranchCommit()
		human := enrichment.PRBranchCommits[7][0]
		human.StatsVerified = true // verified zero at sync time, persisted
		enrichment.PRBranchCommits[7][0] = human

		result := EvaluateCommit(commit, enrichment, exemptBotAuthors(), nil, nil)
		assert.True(t, result.IsCompliant,
			"offline re-audit must honour sync-verified emptiness instead of flapping to non-compliant")
	})
}
