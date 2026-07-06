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

// GitHub mutates a dismissed review IN PLACE: state flips to DISMISSED
// while submitted_at/commit_id keep their original submission values (the
// dismissal time lives only in timeline events we don't fetch). A
// DISMISSED row submitted pre-merge is therefore ambiguous — the
// dismissal could have happened before OR after merge. Fail closed (no
// approval) but surface the ambiguity as a post-merge concern so an
// auditor reviews it instead of the verdict silently flipping.
func TestDismissedReview_PreMergeSubmittedIsSurfacedNotSilent(t *testing.T) {
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	merged := at.Add(time.Hour)
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "squash", AuthorID: 1, AuthorLogin: "author",
		Additions: 10, Deletions: 2, ParentCount: 1, CommittedAt: merged,
	}
	enrichment := model.EnrichmentResult{
		Commit: commit,
		PRs: []model.PullRequest{{
			Org: "o", Repo: "r", Number: 1, Merged: true, HeadSHA: "head",
			AuthorID: 1, MergedAt: merged,
		}},
		Reviews: []model.Review{{
			PRNumber: 1, ReviewID: 9, ReviewerID: 42, ReviewerLogin: "rev",
			State: "DISMISSED", CommitID: "head", SubmittedAt: at, // pre-merge timestamp
		}},
	}

	result := EvaluateCommit(commit, enrichment, nil, nil, nil)

	assert.False(t, result.IsCompliant, "an ambiguous dismissed review must not count as approval")
	assert.True(t, result.HasPostMergeConcern,
		"the dismissal may have happened post-merge; auditors must see it")
}

// Deleted GitHub accounts surface as the ghost user (id 10137) on PR and
// review user fields — a shared sentinel, not a real identity. It must be
// distrusted exactly like an unresolved (zero) ID: a ghost approval can't
// satisfy §4, and two different deleted people must not compare as the
// same user.
func TestGhostUser_IsNeverTrusted(t *testing.T) {
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	merged := at.Add(time.Hour)
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "squash", AuthorID: model.GhostUserID, AuthorLogin: "ghost",
		Additions: 10, Deletions: 2, ParentCount: 1, CommittedAt: merged,
	}
	enrichment := model.EnrichmentResult{
		Commit: commit,
		PRs: []model.PullRequest{{
			Org: "o", Repo: "r", Number: 1, Merged: true, HeadSHA: "head",
			AuthorID: model.GhostUserID, MergedAt: merged,
		}},
		Reviews: []model.Review{{
			PRNumber: 1, ReviewID: 9, ReviewerID: model.GhostUserID, ReviewerLogin: "ghost",
			State: "APPROVED", CommitID: "head", SubmittedAt: at,
		}},
	}

	result := EvaluateCommit(commit, enrichment, nil, nil, nil)
	assert.False(t, result.IsCompliant,
		"an approval attributed to the ghost user is unverifiable and must not pass §4")
	assert.False(t, result.IsSelfApproved,
		"ghost==ghost is two unknown identities, not a proven self-approval")
}

// A required check whose only runs are queued/in_progress hasn't FAILED —
// it just hasn't concluded. Reporting "failure" sent auditors chasing a
// nonexistent red build; "missing" is the accurate pending label (the
// commit stays non-compliant either way).
func TestRequiredChecks_QueuedOnlyIsMissingNotFailure(t *testing.T) {
	required := []RequiredCheck{{Name: "Owner Approval", Conclusion: "success"}}
	runs := []model.CheckRun{
		{CommitSHA: "head", CheckName: "Owner Approval", Status: "in_progress", CheckRunID: 1},
	}
	assert.Equal(t, "missing", evaluateRequiredChecks(runs, "head", required))
}

// A dismissal SUPERSEDED by a later re-approval is moot: the standing
// approval covers the merge, so no concern flag — flagging every
// dismiss-then-reapprove cycle would bury real signals in noise. And an
// unmerged PR has no merge for a dismissal to be ambiguous against.
func TestDismissedReview_SupersededOrUnmergedStaysQuiet(t *testing.T) {
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	pr := model.PullRequest{Number: 1, HeadSHA: "head", MergedAt: at.Add(time.Hour)}

	t.Run("re-approved after dismissal", func(t *testing.T) {
		reviews := []model.Review{
			{PRNumber: 1, ReviewID: 1, ReviewerID: 42, State: "DISMISSED", CommitID: "head", SubmittedAt: at},
			{PRNumber: 1, ReviewID: 2, ReviewerID: 42, State: "APPROVED", CommitID: "head", SubmittedAt: at.Add(10 * time.Minute)},
		}
		latest, concern := latestReviewStatesOnFinal(reviews, pr)
		require.Len(t, latest, 1)
		assert.False(t, concern, "a superseded dismissal is moot")
	})

	t.Run("dismissal on unmerged PR", func(t *testing.T) {
		open := pr
		open.MergedAt = time.Time{}
		reviews := []model.Review{
			{PRNumber: 1, ReviewID: 1, ReviewerID: 42, State: "DISMISSED", CommitID: "head", SubmittedAt: at},
		}
		_, concern := latestReviewStatesOnFinal(reviews, open)
		assert.False(t, concern, "post-merge concern is meaningless without a merge")
	})
}

// idExempt is the id-matching core of the §1 exemption. Only a trusted
// numeric id matches: an unresolved id (0) and the shared ghost id
// (every deleted account) identify no one, so they never match. There
// is no forgeable email fallback.
func TestExemptCommit_IDOnly(t *testing.T) {
	exempt := []model.ExemptAuthor{{Login: "svc", ID: 4242}}

	assert.True(t, idExempt(4242, exempt),
		"a trusted id match is the only way to be exempt")
	assert.False(t, idExempt(0, exempt),
		"an unresolved id (0) can never be exempt — no forgeable email fallback")
	assert.False(t, idExempt(model.GhostUserID, exempt),
		"the shared ghost id identifies no one and is never exempt")
	assert.False(t, idExempt(9999, exempt),
		"a non-matching trusted id is not exempt")
}

// With the dismissal time resolved from timeline events, the §4 verdict
// is exact instead of fail-closed: a post-merge dismissal restores the
// approval the review WAS at merge time (point-in-time doctrine) and
// flags the concern; a known pre-merge dismissal is an unambiguous
// non-approval with nothing to flag.
func TestDismissedReview_ResolvedTimesAreExact(t *testing.T) {
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	merged := at.Add(time.Hour)
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "squash", AuthorID: 1, AuthorLogin: "author",
		Additions: 10, Deletions: 2, ParentCount: 1, CommittedAt: merged,
	}
	base := model.EnrichmentResult{
		Commit: commit,
		PRs: []model.PullRequest{{
			Org: "o", Repo: "r", Number: 1, Merged: true, HeadSHA: "head",
			AuthorID: 1, MergedAt: merged,
		}},
	}

	t.Run("post-merge dismissal restores the merge-time approval", func(t *testing.T) {
		e := base
		e.Reviews = []model.Review{{
			PRNumber: 1, ReviewID: 9, ReviewerID: 42, ReviewerLogin: "rev",
			State: "DISMISSED", CommitID: "head", SubmittedAt: at,
			DismissedAt: merged.Add(48 * time.Hour), DismissedState: "approved",
		}}
		result := EvaluateCommit(commit, e, nil, nil, nil)
		assert.True(t, result.IsCompliant,
			"the review WAS an approval at merge time; a later dismissal must not rewrite history")
		assert.True(t, result.HasPostMergeConcern, "the dismissal itself is the post-merge concern")
	})

	t.Run("known pre-merge dismissal is quiet and non-compliant", func(t *testing.T) {
		e := base
		e.Reviews = []model.Review{{
			PRNumber: 1, ReviewID: 9, ReviewerID: 42, ReviewerLogin: "rev",
			State: "DISMISSED", CommitID: "head", SubmittedAt: at,
			DismissedAt: at.Add(10 * time.Minute), DismissedState: "approved", // before merge
		}}
		result := EvaluateCommit(commit, e, nil, nil, nil)
		assert.False(t, result.IsCompliant, "the approval was rescinded before the merge")
		assert.False(t, result.HasPostMergeConcern, "a resolved pre-merge dismissal is not ambiguous")
	})

	t.Run("post-merge dismissal of CHANGES_REQUESTED does not mint an approval", func(t *testing.T) {
		e := base
		e.Reviews = []model.Review{{
			PRNumber: 1, ReviewID: 9, ReviewerID: 42, ReviewerLogin: "rev",
			State: "DISMISSED", CommitID: "head", SubmittedAt: at,
			DismissedAt: merged.Add(time.Hour), DismissedState: "changes_requested",
		}}
		result := EvaluateCommit(commit, e, nil, nil, nil)
		assert.False(t, result.IsCompliant,
			"at merge time the reviewer was requesting changes — restoring that state is not an approval")
	})
}

// The §4 stale-approval carve-out's "post-approval" set is decided by
// GRAPH POSITION when parent data exists: the first-parent walk from the
// PR head to the approved CommitID. Backdating GIT_COMMITTER_DATE moved a
// commit out of the old temporal set; it cannot move out of the ancestry
// path.
func TestApprovalRefreshable_PositionalNotTemporal(t *testing.T) {
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	merged := at.Add(2 * time.Hour)
	bot := []model.ExemptAuthor{{Login: "ci-bot", ID: 99}}

	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "squash", AuthorID: 7, AuthorLogin: "dev",
		Additions: 10, Deletions: 2, ParentCount: 1, CommittedAt: merged,
	}
	pr := model.PullRequest{
		Org: "o", Repo: "r", Number: 1, Merged: true, HeadSHA: "head",
		AuthorID: 7, MergedAt: merged,
	}
	staleApproval := model.Review{
		PRNumber: 1, ReviewID: 9, ReviewerID: 42, ReviewerLogin: "rev",
		State: "APPROVED", CommitID: "approved", SubmittedAt: at,
	}
	// Branch graph: base <- approved <- human(backdated) <- bot(head)
	branch := func(humanCommittedAt time.Time) []model.Commit {
		return []model.Commit{
			{Org: "o", Repo: "r", SHA: "approved", AuthorID: 7, AuthorLogin: "dev",
				CommittedAt: at.Add(-time.Hour), Additions: 5, ParentSHAs: []string{"base"}},
			{Org: "o", Repo: "r", SHA: "human", AuthorID: 7, AuthorLogin: "dev",
				CommittedAt: humanCommittedAt, Additions: 30, ParentSHAs: []string{"approved"}},
			{Org: "o", Repo: "r", SHA: "head", AuthorID: 99, AuthorLogin: "ci-bot",
				CommittedAt: at.Add(time.Hour), Additions: 1, ParentSHAs: []string{"human"}},
		}
	}
	evaluate := func(branchCommits []model.Commit) model.AuditResult {
		e := model.EnrichmentResult{
			Commit:          commit,
			PRs:             []model.PullRequest{pr},
			Reviews:         []model.Review{staleApproval},
			PRBranchCommits: map[int][]model.Commit{1: branchCommits},
		}
		return EvaluateCommit(commit, e, bot, nil, nil)
	}

	t.Run("backdated human commit cannot escape the walk", func(t *testing.T) {
		// Committer date BEFORE the approval — the temporal check would
		// exclude it from the post-approval set and grant the promotion.
		result := evaluate(branch(at.Add(-30 * time.Minute)))
		assert.False(t, result.IsCompliant,
			"a human commit between the approved SHA and head voids the carve-out regardless of its timestamps")
		assert.True(t, result.HasStaleApproval)
	})

	t.Run("bot-only post-approval commits still refresh", func(t *testing.T) {
		commits := branch(at.Add(30 * time.Minute))
		commits[1].AuthorID = 99 // the middle commit is the bot's too
		commits[1].AuthorLogin = "ci-bot"
		result := evaluate(commits)
		assert.True(t, result.IsCompliant,
			"exempt-only post-approval commits keep the approval's coverage")
	})

	t.Run("force-pushed history fails closed", func(t *testing.T) {
		commits := branch(at.Add(30 * time.Minute))
		commits[0].SHA = "rewritten" // approved SHA no longer in the chain
		commits[1].ParentSHAs = []string{"rewritten"}
		result := evaluate(commits)
		assert.False(t, result.IsCompliant,
			"an unreachable approved SHA means history was rewritten — no promotion")
	})

	t.Run("legacy rows without parents fail closed — no temporal trust", func(t *testing.T) {
		// Without parent data there is no non-forgeable way to order commits,
		// so the carve-out must NOT promote the approval. The commit shows as
		// non-compliant (recoverable) until a re-sync supplies parent SHAs.
		commits := branch(at.Add(30 * time.Minute))
		for i := range commits {
			commits[i].ParentSHAs = nil
		}
		commits[1].AuthorID = 99 // even all-exempt-by-time post-approval commits
		commits[1].AuthorLogin = "ci-bot"
		result := evaluate(commits)
		assert.False(t, result.IsCompliant,
			"no parent data → fail closed, never trust forgeable committer timestamps")
		assert.True(t, result.HasStaleApproval)
	})

	t.Run("LEAK: backdated unreviewed commit cannot ride in via the timestamp path", func(t *testing.T) {
		// The attack the temporal fallback enabled: push unreviewed human code
		// AFTER the approval, set GIT_COMMITTER_DATE to before the approval so
		// the timestamp check skips it, and let an exempt bot commit be the
		// only "post-approval" commit by time. With no parent data the old code
		// promoted the stale approval and waived the 30 unreviewed additions.
		commits := branch(at.Add(-30 * time.Minute)) // human commit backdated
		for i := range commits {
			commits[i].ParentSHAs = nil // legacy row: no graph data
		}
		// commits[1] stays AuthorID=7 (human, non-exempt) with 30 additions.
		result := evaluate(commits)
		assert.False(t, result.IsCompliant,
			"a backdated non-exempt commit must never be laundered into compliance")
		assert.True(t, result.HasStaleApproval)
	})
}
