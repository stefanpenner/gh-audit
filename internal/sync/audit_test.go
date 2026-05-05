package sync

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evalCase is one row of the per-rule test tables. Rule test functions
// below mirror Architecture.md §1–§8 one-to-one so a reviewer can match
// the doc to a test function at a glance.
type evalCase struct {
	name                 string
	commit               model.Commit
	enrichment           model.EnrichmentResult
	exemptAuthors        []model.ExemptAuthor
	requiredChecks       []RequiredCheck
	wantCompliant        bool
	wantBot              bool
	wantExempt           bool
	wantEmpty            bool
	wantHasPR            bool
	wantSelfApproved     bool
	wantStaleApproval    bool
	wantPostMergeConcern bool
	// wantReasons may be nil to skip the exact-reason assertion (used for
	// multi-PR cases where the tiebreak picks one of several equivalent
	// best PRs).
	wantReasons []string
}

// runEvalCases drives a table of evalCases through EvaluateCommit and
// applies the shared assertions. Clean-revert signals are asserted as
// pass-through: whatever the enrichment set must land unchanged on the
// result, regardless of which rule fired.
func runEvalCases(t *testing.T, cases []evalCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			result := EvaluateCommit(tt.commit, tt.enrichment, tt.exemptAuthors, tt.requiredChecks, nil)

			assert.Equal(t, tt.wantCompliant, result.IsCompliant, "IsCompliant (reasons: %v)", result.Reasons)
			assert.Equal(t, tt.wantBot, result.IsBot, "IsBot")
			assert.Equal(t, tt.wantExempt, result.IsExemptAuthor, "IsExemptAuthor")
			assert.Equal(t, tt.wantEmpty, result.IsEmptyCommit, "IsEmptyCommit")
			assert.Equal(t, tt.wantHasPR, result.HasPR, "HasPR")
			assert.Equal(t, tt.wantSelfApproved, result.IsSelfApproved, "IsSelfApproved")
			assert.Equal(t, tt.wantStaleApproval, result.HasStaleApproval, "HasStaleApproval")
			assert.Equal(t, tt.wantPostMergeConcern, result.HasPostMergeConcern, "HasPostMergeConcern")
			assert.Equal(t, tt.enrichment.IsCleanRevert, result.IsCleanRevert, "IsCleanRevert")
			assert.Equal(t, tt.enrichment.RevertVerification, result.RevertVerification, "RevertVerification")
			assert.Equal(t, tt.enrichment.RevertedSHA, result.RevertedSHA, "RevertedSHA")
			if tt.wantReasons != nil {
				require.Equal(t, tt.wantReasons, result.Reasons, "Reasons")
			}
		})
	}
}

// auditBaseline bundles the commit/PR/review/check-run fixtures used by
// the rule tables. Tests that reference the normal compliance path pull
// these via newAuditBaseline(); tests that need bespoke values build
// their own model.Commit / model.PullRequest inline.
type auditBaseline struct {
	now                time.Time
	commit             model.Commit
	pr                 model.PullRequest
	approvedReview     model.Review
	ownerApprovalCheck model.CheckRun
	requiredChecks     []RequiredCheck
}

func newAuditBaseline() auditBaseline {
	now := time.Now()
	return auditBaseline{
		now: now,
		commit: model.Commit{
			Org:         "myorg",
			Repo:        "myrepo",
			SHA:         "abc123",
			AuthorLogin: "developer",
			CommittedAt: now,
			Message:     "feat: add feature",
			Additions:   10,
			Deletions:   5,
			Href:        "https://github.com/myorg/myrepo/commit/abc123",
		},
		pr: model.PullRequest{
			Org:            "myorg",
			Repo:           "myrepo",
			Number:         42,
			Title:          "Add feature",
			Merged:         true,
			HeadSHA:        "head123",
			MergeCommitSHA: "abc123",
			AuthorLogin:    "developer",
			MergedAt:       now,
			Href:           "https://github.com/myorg/myrepo/pull/42",
		},
		approvedReview: model.Review{
			Org:           "myorg",
			Repo:          "myrepo",
			PRNumber:      42,
			ReviewID:      1,
			ReviewerLogin: "reviewer1",
			State:         "APPROVED",
			CommitID:      "head123",
			SubmittedAt:   now,
		},
		ownerApprovalCheck: model.CheckRun{
			Org:        "myorg",
			Repo:       "myrepo",
			CommitSHA:  "head123",
			CheckRunID: 100,
			CheckName:  "Owner Approval",
			Status:     "completed",
			Conclusion: "success",
		},
		requiredChecks: []RequiredCheck{
			{Name: "Owner Approval", Conclusion: "success"},
		},
	}
}

// TestEvaluateCommit_Rule1_ExemptAuthor covers Architecture.md §1.
//
// If the commit author's GitHub login is in exemptions.authors
// (case-insensitive), the commit is compliant immediately. The IsBot
// flag is an orthogonal informational signal (login ending in "[bot]")
// and fires regardless of exemption membership.
func TestEvaluateCommit_Rule1_ExemptAuthor(t *testing.T) {
	cases := []evalCase{
		{
			name:          "exempt author is compliant",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "dependabot[bot]", AuthorID: 49699333, Additions: 5, Deletions: 3},
			enrichment:    model.EnrichmentResult{},
			exemptAuthors: []model.ExemptAuthor{
				{Login: "dependabot[bot]", ID: 49699333},
				{Login: "renovate[bot]", ID: 2740337},
			},
			wantCompliant: true,
			wantBot:       true,
			wantExempt:    true,
			wantReasons:   []string{"exempt: configured author"},
		},
		{
			name:          "bot not in exempt list is not exempt",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "some-ci[bot]", Additions: 5, Deletions: 3},
			enrichment:    model.EnrichmentResult{},
			exemptAuthors: []model.ExemptAuthor{{Login: "dependabot[bot]", ID: 49699333}},
			wantCompliant: false,
			wantBot:       true,
			wantExempt:    false,
			wantReasons:   []string{"no associated pull request"},
		},
		{
			name:          "non-bot exempt author is exempt but not bot",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "service-account", AuthorID: 88001, Additions: 5, Deletions: 3},
			enrichment:    model.EnrichmentResult{},
			exemptAuthors: []model.ExemptAuthor{{Login: "service-account", ID: 88001}},
			wantCompliant: true,
			wantBot:       false,
			wantExempt:    true,
			wantReasons:   []string{"exempt: configured author"},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule2_EmptyCommit covers Architecture.md §2.
//
// A commit with zero additions AND zero deletions is compliant, flagged
// for visibility as IsEmptyCommit. Stats are resolved lazily — see
// TestEvaluateCommit_LazyStatsFetcher for the fetcher contract.
func TestEvaluateCommit_Rule2_EmptyCommit(t *testing.T) {
	cases := []evalCase{
		{
			name:          "empty commit is exempt",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "developer", Additions: 0, Deletions: 0},
			enrichment:    model.EnrichmentResult{},
			wantCompliant: true,
			wantEmpty:     true,
			wantReasons:   []string{"empty commit"},
		},
		{
			name:          "commit with additions is not empty",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "developer", Additions: 42, Deletions: 0},
			enrichment:    model.EnrichmentResult{},
			wantCompliant: false,
			wantEmpty:     false,
			wantReasons:   []string{"no associated pull request"},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule3_HasAssociatedPR covers Architecture.md §3.
//
// A commit with no merged PR (direct push) is non-compliant with reason
// "no associated pull request" unless rule 2 (empty commit) applies.
func TestEvaluateCommit_Rule3_HasAssociatedPR(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:          "no PR is non-compliant",
			commit:        f.commit,
			enrichment:    model.EnrichmentResult{},
			wantCompliant: false,
			wantHasPR:     false,
			wantReasons:   []string{"no associated pull request"},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule4_ApprovalOnFinal covers Architecture.md §4
// per-reviewer latest-state resolution on pr.HeadSHA. DISMISSED /
// CHANGES_REQUESTED supersede an earlier APPROVED from the same
// reviewer; a later COMMENTED does NOT revoke an earlier APPROVED
// (matches GitHub's UI, where commenting after approving leaves the
// approval intact).
func TestEvaluateCommit_Rule4_ApprovalOnFinal(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:   "PR exists but no reviews",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{f.pr},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "CHANGES_REQUESTED then APPROVED on final commit",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED", CommitID: "head123", SubmittedAt: f.now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer2", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "re-approval after force-push same reviewer",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-force-pushed-sha", SubmittedAt: f.now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "DISMISSED review on final commit is non-compliant",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "DISMISSED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "APPROVED then DISMISSED on final commit is non-compliant",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "DISMISSED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "APPROVED then COMMENTED on final commit is still compliant",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "COMMENTED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "mixed states on final CHANGES_REQUESTED and APPROVED from different reviewers",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED", CommitID: "head123", SubmittedAt: f.now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer2", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			// Both reviews submitted before merge (MergedAt is 5 minutes
			// out), so the second CHANGES_REQUESTED clobbers the earlier
			// APPROVED via per-reviewer latest-state. HasPostMergeConcern
			// stays false because neither review is post-merge.
			name:   "reviewer flipped APPROVED then CHANGES_REQUESTED before merge",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{func() model.PullRequest {
					p := f.pr
					p.MergedAt = f.now.Add(5 * time.Minute)
					return p
				}()},
				Reviews: []model.Review{
					f.approvedReview, // APPROVED at `now`
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 6,
						ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED",
						CommitID: "head123", SubmittedAt: f.now.Add(2 * time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:       f.requiredChecks,
			wantCompliant:        false,
			wantHasPR:            true,
			wantPostMergeConcern: false,
			wantReasons:          []string{"no approval on final commit (PR #42)"},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule4_StaleApproval covers Architecture.md §4 stale
// detection: when no non-self APPROVED exists on pr.HeadSHA but an
// APPROVED from a non-self reviewer exists on an older SHA, the reason
// is "approval is stale — not on final commit" instead of "no approval
// on final commit". Self-approvals never count as stale.
func TestEvaluateCommit_Rule4_StaleApproval(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:   "review on non-final commit",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-commit-sha"},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:    f.requiredChecks,
			wantCompliant:     false,
			wantHasPR:         true,
			wantStaleApproval: true,
			wantReasons:       []string{"approval is stale — not on final commit (PR #42)"},
		},
		{
			name:   "stale approval after force-push",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-force-pushed-sha", SubmittedAt: f.now.Add(-time.Hour)},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:    f.requiredChecks,
			wantCompliant:     false,
			wantHasPR:         true,
			wantStaleApproval: true,
			wantReasons:       []string{"approval is stale — not on final commit (PR #42)"},
		},
		{
			name:   "stale approval from one reviewer fresh approval from another",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-force-pushed-sha", SubmittedAt: f.now.Add(-2 * time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer2", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "same reviewer old APPROVED then CHANGES_REQUESTED on final",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-sha", SubmittedAt: f.now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:    f.requiredChecks,
			wantCompliant:     false,
			wantHasPR:         true,
			wantStaleApproval: true,
			wantReasons:       []string{"approval is stale — not on final commit (PR #42)"},
		},
		{
			// Self-approval on an old SHA must not fire HasStaleApproval —
			// "stale" implies a legitimate-but-outdated review, not a
			// disqualified one.
			name:   "only self-approval on old SHA is not flagged as stale",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "developer", State: "APPROVED", CommitID: "old-sha", SubmittedAt: f.now.Add(-time.Hour)},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			// Both flaws present: an independent reviewer approved on an
			// older SHA (→ staleApproval), then the PR/commit author
			// self-approved on the final SHA (→ selfApproved,
			// !approvalOnFinal). Both reasons must be emitted so reports
			// align with HasStaleApproval and IsSelfApproved flags.
			name:   "self-approval on final SHA plus independent stale approval on old SHA",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-sha", SubmittedAt: f.now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "developer", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:    f.requiredChecks,
			wantCompliant:     false,
			wantHasPR:         true,
			wantSelfApproved:  true,
			wantStaleApproval: true,
			wantReasons: []string{
				"self-approved (reviewer is code author) (PR #42)",
				"approval is stale — not on final commit (PR #42)",
			},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule4_PostMergeCutoff covers Architecture.md §4
// post-merge cutoff. Reviews submitted after pr.MergedAt are excluded
// from compliance; a post-merge DISMISSED or CHANGES_REQUESTED instead
// sets HasPostMergeConcern (informational, does not flip compliance).
// Open PRs (no MergedAt) apply no cutoff.
func TestEvaluateCommit_Rule4_PostMergeCutoff(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:   "approved before merge, CHANGES_REQUESTED after merge → compliant + post-merge concern",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					f.approvedReview, // submitted at `now`, matches pr.MergedAt — included (After is strict)
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2,
						ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED",
						CommitID: "head123", SubmittedAt: f.now.Add(time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:       f.requiredChecks,
			wantCompliant:        true,
			wantHasPR:            true,
			wantPostMergeConcern: true,
			wantReasons:          []string{"compliant"},
		},
		{
			name:   "approved before merge, DISMISSED after merge → compliant + post-merge concern",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					f.approvedReview,
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 3,
						ReviewerLogin: "reviewer1", State: "DISMISSED",
						CommitID: "head123", SubmittedAt: f.now.Add(2 * time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:       f.requiredChecks,
			wantCompliant:        true,
			wantHasPR:            true,
			wantPostMergeConcern: true,
			wantReasons:          []string{"compliant"},
		},
		{
			name:   "approved before merge, COMMENTED after merge → compliant, no concern",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					f.approvedReview,
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 4,
						ReviewerLogin: "reviewer2", State: "COMMENTED",
						CommitID: "head123", SubmittedAt: f.now.Add(time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:       f.requiredChecks,
			wantCompliant:        true,
			wantHasPR:            true,
			wantPostMergeConcern: false,
			wantReasons:          []string{"compliant"},
		},
		{
			name:   "open PR (no MergedAt) with later CHANGES_REQUESTED → no concern, non-compliant",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{func() model.PullRequest {
					p := f.pr
					p.Merged = false
					p.MergedAt = time.Time{}
					return p
				}()},
				Reviews: []model.Review{
					f.approvedReview,
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 5,
						ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED",
						CommitID: "head123", SubmittedAt: f.now.Add(time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:       f.requiredChecks,
			wantCompliant:        false,
			wantHasPR:            true,
			wantPostMergeConcern: false,
			wantReasons:          []string{"no approval on final commit (PR #42)"},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule5_SelfApproval covers Architecture.md §5.
//
// A review is self-approval when the reviewer matches any of: PR author,
// commit author (skipped for CleanMerge), committer (skipped for
// CleanMerge; web-flow/github always excluded), or any Co-authored-by
// trailer. For squash merges the check extends to all PR branch commit
// authors — see TestSquashMergePRCommitAuthors for deeper coverage.
func TestEvaluateCommit_Rule5_SelfApproval(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:   "PR author == reviewer is self-approval",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "developer", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:   f.requiredChecks,
			wantCompliant:    false,
			wantHasPR:        true,
			wantSelfApproved: true,
			wantReasons:      []string{"self-approved (reviewer is code author) (PR #42)"},
		},
		{
			name: "commit author == reviewer is self-approval",
			commit: model.Commit{
				Org: "myorg", Repo: "myrepo", SHA: "abc123",
				AuthorLogin: "coder", Additions: 10, Deletions: 5,
				Href: "https://github.com/myorg/myrepo/commit/abc123",
			},
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 42, HeadSHA: "head123", MergeCommitSHA: "abc123", AuthorLogin: "prauthor", Href: "https://github.com/myorg/myrepo/pull/42"},
				},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "coder", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:   f.requiredChecks,
			wantCompliant:    false,
			wantHasPR:        true,
			wantSelfApproved: true,
			wantReasons:      []string{"self-approved (reviewer is code author) (PR #42)"},
		},
		{
			name: "co-author == reviewer is self-approval",
			commit: model.Commit{
				Org: "myorg", Repo: "myrepo", SHA: "abc123",
				AuthorLogin: "maindev", Additions: 10, Deletions: 5,
				CoAuthors: []model.CoAuthor{{Login: "codev", Email: "codev@example.com"}},
				Href:      "https://github.com/myorg/myrepo/commit/abc123",
			},
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 42, HeadSHA: "head123", MergeCommitSHA: "abc123", AuthorLogin: "maindev", Href: "https://github.com/myorg/myrepo/pull/42"},
				},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "codev", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:   f.requiredChecks,
			wantCompliant:    false,
			wantHasPR:        true,
			wantSelfApproved: true,
			wantReasons:      []string{"self-approved (reviewer is code author) (PR #42)"},
		},
		{
			name: "committer == reviewer (non-bot) is self-approval",
			commit: model.Commit{
				Org: "myorg", Repo: "myrepo", SHA: "abc123",
				AuthorLogin: "maindev", CommitterLogin: "deployer",
				Additions: 10, Deletions: 5,
				Href: "https://github.com/myorg/myrepo/commit/abc123",
			},
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 42, HeadSHA: "head123", MergeCommitSHA: "abc123", AuthorLogin: "maindev", Href: "https://github.com/myorg/myrepo/pull/42"},
				},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "deployer", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:   f.requiredChecks,
			wantCompliant:    false,
			wantHasPR:        true,
			wantSelfApproved: true,
			wantReasons:      []string{"self-approved (reviewer is code author) (PR #42)"},
		},
		{
			name: "committer is web-flow and matches reviewer is NOT self-approval",
			commit: model.Commit{
				Org: "myorg", Repo: "myrepo", SHA: "abc123",
				AuthorLogin: "maindev", CommitterLogin: "web-flow",
				Additions: 10, Deletions: 5,
				Href: "https://github.com/myorg/myrepo/commit/abc123",
			},
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 42, HeadSHA: "head123", MergeCommitSHA: "abc123", AuthorLogin: "maindev", Href: "https://github.com/myorg/myrepo/pull/42"},
				},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "web-flow", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			// Self-approval never disqualifies when an independent
			// approval also exists — a co-author review is redundant, not
			// fatal.
			name:   "self-approval exists but another non-self approval also exists is compliant",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "developer", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "independent-reviewer", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			// CleanMerge: author/committer is "who clicked merge", not a
			// code author. Reviewer == merger must stay clean.
			name: "CleanMerge: commit author is merger not code author — not self-approval",
			commit: model.Commit{
				Org: "myorg", Repo: "myrepo", SHA: "mergeabc",
				AuthorLogin: "merger", CommitterLogin: "web-flow",
				IsVerified: true,
				Additions:  10, Deletions: 5, ParentCount: 2,
				Message: "Merge pull request #42 from codeauthor/feature",
				Href:    "https://github.com/myorg/myrepo/commit/mergeabc",
			},
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 42, HeadSHA: "head123", MergeCommitSHA: "mergeabc", AuthorLogin: "codeauthor", Href: "https://github.com/myorg/myrepo/pull/42"},
				},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "merger", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name: "non-merge commit author == reviewer is still self-approval",
			commit: model.Commit{
				Org: "myorg", Repo: "myrepo", SHA: "abc123",
				AuthorLogin: "coder", CommitterLogin: "web-flow",
				Additions: 10, Deletions: 5, ParentCount: 1,
				Href: "https://github.com/myorg/myrepo/commit/abc123",
			},
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 42, HeadSHA: "head123", MergeCommitSHA: "abc123", AuthorLogin: "prauthor", Href: "https://github.com/myorg/myrepo/pull/42"},
				},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "coder", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks:   f.requiredChecks,
			wantCompliant:    false,
			wantHasPR:        true,
			wantSelfApproved: true,
			wantReasons:      []string{"self-approved (reviewer is code author) (PR #42)"},
		},
		{
			name: "multiple reviewers one self one legitimate is compliant",
			commit: model.Commit{
				Org: "myorg", Repo: "myrepo", SHA: "abc123",
				AuthorLogin: "developer", CommitterLogin: "deployer",
				Additions: 10, Deletions: 5,
				CoAuthors: []model.CoAuthor{{Login: "codev"}},
				Href:      "https://github.com/myorg/myrepo/commit/abc123",
			},
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "codev", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "external-reviewer", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule6_RequiredChecks covers Architecture.md §6.
//
// Configured required checks must appear on pr.HeadSHA with the expected
// conclusion. Missing or failed required checks make the commit
// non-compliant even when rule 4 passed. See TestEvaluateRequiredChecks
// for the helper-level "success" / "failure" / "missing" matrix.
func TestEvaluateCommit_Rule6_RequiredChecks(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:   "approved on final but Owner Approval missing",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{f.pr},
				Reviews: []model.Review{f.approvedReview},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"Owner Approval check missing/failed (PR #42)"},
		},
		{
			name:   "approved on final but Owner Approval failed",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{f.pr},
				Reviews: []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{
					{Org: "myorg", Repo: "myrepo", CommitSHA: "head123", CheckRunID: 100, CheckName: "Owner Approval", Status: "completed", Conclusion: "failure"},
				},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"Owner Approval check missing/failed (PR #42)"},
		},
		{
			name:   "no required checks means Owner Approval not needed",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{f.pr},
				Reviews: []model.Review{f.approvedReview},
			},
			requiredChecks: nil,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "multiple required checks all pass",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{f.pr},
				Reviews: []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{
					{Org: "myorg", Repo: "myrepo", CommitSHA: "head123", CheckRunID: 100, CheckName: "Owner Approval", Status: "completed", Conclusion: "success"},
					{Org: "myorg", Repo: "myrepo", CommitSHA: "head123", CheckRunID: 101, CheckName: "lint", Status: "completed", Conclusion: "success"},
				},
			},
			requiredChecks: []RequiredCheck{
				{Name: "Owner Approval", Conclusion: "success"},
				{Name: "lint", Conclusion: "success"},
			},
			wantCompliant: true,
			wantHasPR:     true,
			wantReasons:   []string{"compliant"},
		},
		{
			name:   "multiple required checks one fails",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{f.pr},
				Reviews: []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{
					{Org: "myorg", Repo: "myrepo", CommitSHA: "head123", CheckRunID: 100, CheckName: "Owner Approval", Status: "completed", Conclusion: "success"},
					{Org: "myorg", Repo: "myrepo", CommitSHA: "head123", CheckRunID: 101, CheckName: "lint", Status: "completed", Conclusion: "failure"},
				},
			},
			requiredChecks: []RequiredCheck{
				{Name: "Owner Approval", Conclusion: "success"},
				{Name: "lint", Conclusion: "success"},
			},
			wantCompliant: false,
			wantHasPR:     true,
			wantReasons:   []string{"Owner Approval check missing/failed (PR #42)"},
		},
		{
			name:   "multiple required checks one missing one passes",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{f.pr},
				Reviews: []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{
					{Org: "myorg", Repo: "myrepo", CommitSHA: "head123", CheckRunID: 100, CheckName: "Owner Approval", Status: "completed", Conclusion: "success"},
				},
			},
			requiredChecks: []RequiredCheck{
				{Name: "Owner Approval", Conclusion: "success"},
				{Name: "lint", Conclusion: "success"},
			},
			wantCompliant: false,
			wantHasPR:     true,
			wantReasons:   []string{"Owner Approval check missing/failed (PR #42)"},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule7_Verdict covers Architecture.md §7 — the
// single- and multi-PR compliance verdict. Also covers the merge-commit
// baseline: a 2-parent commit follows the same flow as a squash merge.
// If multiple PRs exist and at least one passes rules 4+6 the commit is
// compliant; otherwise the best PR (fewest reasons; higher number on
// ties) drives the non-compliant reasons.
func TestEvaluateCommit_Rule7_Verdict(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:   "single approved PR is compliant (baseline)",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{f.pr},
				Reviews:   []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "multiple PRs first non-compliant second compliant",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 41, HeadSHA: "head-old", Href: "https://github.com/myorg/myrepo/pull/41"},
					f.pr,
				},
				Reviews:   []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			// wantReasons intentionally nil — either PR is a valid "best"
			// choice since both have equal non-compliant reasons; the
			// tiebreak (higher PR number) picks #42.
			name:   "multiple PRs all non-compliant",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 41, HeadSHA: "head-old", Href: "https://github.com/myorg/myrepo/pull/41"},
					{Org: "myorg", Repo: "myrepo", Number: 42, HeadSHA: "head123", Href: "https://github.com/myorg/myrepo/pull/42"},
				},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
		},
		{
			name:   "merge commit treated normally",
			commit: model.Commit{Org: "myorg", Repo: "myrepo", SHA: "merge123", AuthorLogin: "developer", ParentCount: 2, Additions: 10, Deletions: 5, Href: "https://github.com/myorg/myrepo/commit/merge123"},
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{f.pr},
				Reviews:   []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
	}
	runEvalCases(t, cases)
}

// TestEvaluateCommit_Rule8_SignalPassThrough covers the informational
// copy-through of revert signals (IsCleanRevert, RevertVerification,
// RevertedSHA) from EnrichmentResult to AuditResult. These fields land
// on the result regardless of which rule fired — they're displayed by
// the report even when compliance was granted by the PR-approval path.
//
// See TestEvaluateCommit_RevertWaivers for end-to-end §8 waiver coverage
// (where IsCleanRevert is load-bearing for compliance) and
// TestEvaluateRevertCompliance for the helper-level tests.
func TestEvaluateCommit_Rule8_SignalPassThrough(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:   "enrichment marks auto-revert clean — result carries the flag",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{f.pr},
				Reviews:   []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},

				IsCleanRevert:      true,
				RevertVerification: "message-only",
				RevertedSHA:        "abcdef1234567890abcdef1234567890abcdef12",
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "enrichment marks diff-verified manual revert — result carries the flag",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{f.pr},
				Reviews:   []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},

				IsCleanRevert:      true,
				RevertVerification: "diff-verified",
				RevertedSHA:        "1234567890abcdef1234567890abcdef12345678",
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "enrichment marks diff-mismatch — IsCleanRevert stays false but verification recorded",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{f.pr},
				Reviews:   []model.Review{f.approvedReview},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},

				IsCleanRevert:      false,
				RevertVerification: "diff-mismatch",
				RevertedSHA:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
	}
	runEvalCases(t, cases)
}

func TestEvaluateRequiredChecks(t *testing.T) {
	checks := []model.CheckRun{
		{CommitSHA: "head1", CheckName: "lint", Conclusion: "success"},
		{CommitSHA: "head1", CheckName: "test", Conclusion: "failure"},
		{CommitSHA: "head1", CheckName: "build", Conclusion: "success"},
		{CommitSHA: "other", CheckName: "lint", Conclusion: "success"},
	}

	tests := []struct {
		name           string
		headSHA        string
		requiredChecks []RequiredCheck
		want           string
	}{
		{
			name:           "no required checks",
			headSHA:        "head1",
			requiredChecks: nil,
			want:           "",
		},
		{
			name:    "single check passes",
			headSHA: "head1",
			requiredChecks: []RequiredCheck{
				{Name: "lint", Conclusion: "success"},
			},
			want: "success",
		},
		{
			name:    "single check fails",
			headSHA: "head1",
			requiredChecks: []RequiredCheck{
				{Name: "test", Conclusion: "success"},
			},
			want: "failure",
		},
		{
			name:    "single check missing",
			headSHA: "head1",
			requiredChecks: []RequiredCheck{
				{Name: "deploy", Conclusion: "success"},
			},
			want: "missing",
		},
		{
			name:    "all pass",
			headSHA: "head1",
			requiredChecks: []RequiredCheck{
				{Name: "lint", Conclusion: "success"},
				{Name: "build", Conclusion: "success"},
			},
			want: "success",
		},
		{
			name:    "one passes one fails",
			headSHA: "head1",
			requiredChecks: []RequiredCheck{
				{Name: "lint", Conclusion: "success"},
				{Name: "test", Conclusion: "success"},
			},
			want: "failure",
		},
		{
			name:    "one passes one missing",
			headSHA: "head1",
			requiredChecks: []RequiredCheck{
				{Name: "lint", Conclusion: "success"},
				{Name: "deploy", Conclusion: "success"},
			},
			want: "missing",
		},
		{
			name:    "failure takes precedence over missing",
			headSHA: "head1",
			requiredChecks: []RequiredCheck{
				{Name: "test", Conclusion: "success"},
				{Name: "deploy", Conclusion: "success"},
			},
			want: "failure",
		},
		{
			name:    "wrong SHA no match",
			headSHA: "wrong",
			requiredChecks: []RequiredCheck{
				{Name: "lint", Conclusion: "success"},
			},
			want: "missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateRequiredChecks(checks, tt.headSHA, tt.requiredChecks)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSquashMergePRCommitAuthors(t *testing.T) {
	now := time.Now()

	squashCommit := model.Commit{
		Org:         "myorg",
		Repo:        "myrepo",
		SHA:         "squash1",
		AuthorLogin: "dev-a",
		CommittedAt: now,
		Message:     "feat: squash merged",
		ParentCount: 1,
		Additions:   10,
		Deletions:   5,
		Href:        "https://github.com/myorg/myrepo/commit/squash1",
	}

	pr := model.PullRequest{
		Org:         "myorg",
		Repo:        "myrepo",
		Number:      10,
		HeadSHA:     "head1",
		AuthorLogin: "dev-a",
		Merged:      true,
		Href:        "https://github.com/myorg/myrepo/pull/10",
	}

	approvalFromDevB := model.Review{
		Org:           "myorg",
		Repo:          "myrepo",
		PRNumber:      10,
		ReviewID:      1,
		ReviewerLogin: "dev-b",
		State:         "APPROVED",
		CommitID:      "head1",
		SubmittedAt:   now,
	}

	approvalFromIndependent := model.Review{
		Org:           "myorg",
		Repo:          "myrepo",
		PRNumber:      10,
		ReviewID:      2,
		ReviewerLogin: "independent-reviewer",
		State:         "APPROVED",
		CommitID:      "head1",
		SubmittedAt:   now,
	}

	t.Run("reviewer is PR commit author — self-approved", func(t *testing.T) {
		enrichment := model.EnrichmentResult{
			Commit:  squashCommit,
			PRs:     []model.PullRequest{pr},
			Reviews: []model.Review{approvalFromDevB},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dev-a"},
					{Org: "myorg", Repo: "myrepo", SHA: "c2", AuthorLogin: "dev-b"},
				},
			},
		}

		result := EvaluateCommit(squashCommit, enrichment, nil, nil, nil)
		assert.True(t, result.IsSelfApproved, "dev-b approved but also contributed commits")
		assert.False(t, result.IsCompliant)
		assert.Contains(t, result.PRCommitAuthorLogins, "dev-a")
		assert.Contains(t, result.PRCommitAuthorLogins, "dev-b")
	})

	t.Run("reviewer is independent — compliant", func(t *testing.T) {
		enrichment := model.EnrichmentResult{
			Commit:  squashCommit,
			PRs:     []model.PullRequest{pr},
			Reviews: []model.Review{approvalFromIndependent},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dev-a"},
					{Org: "myorg", Repo: "myrepo", SHA: "c2", AuthorLogin: "dev-b"},
				},
			},
		}

		result := EvaluateCommit(squashCommit, enrichment, nil, nil, nil)
		assert.False(t, result.IsSelfApproved)
		assert.True(t, result.IsCompliant)
	})

	t.Run("exempt bot + human contributor, no review — non-compliant", func(t *testing.T) {
		botCommit := squashCommit
		botCommit.AuthorLogin = "dependabot[bot]"
		botCommit.AuthorID = 49699333

		botPR := pr
		botPR.AuthorLogin = "dependabot[bot]"

		enrichment := model.EnrichmentResult{
			Commit: botCommit,
			PRs:    []model.PullRequest{botPR},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dependabot[bot]", AuthorID: 49699333},
					{Org: "myorg", Repo: "myrepo", SHA: "c2", AuthorLogin: "human-dev", AuthorID: 1234567},
				},
			},
		}

		result := EvaluateCommit(botCommit, enrichment, []model.ExemptAuthor{{Login: "dependabot[bot]", ID: 49699333}}, nil, nil)
		assert.True(t, result.IsExemptAuthor, "commit author is exempt")
		assert.False(t, result.IsCompliant, "non-exempt contributor needs review")
	})

	t.Run("exempt bot + human contributor, reviewed — compliant", func(t *testing.T) {
		botCommit := squashCommit
		botCommit.AuthorLogin = "dependabot[bot]"
		botCommit.AuthorID = 49699333

		botPR := pr
		botPR.AuthorLogin = "dependabot[bot]"

		enrichment := model.EnrichmentResult{
			Commit:  botCommit,
			PRs:     []model.PullRequest{botPR},
			Reviews: []model.Review{approvalFromIndependent},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dependabot[bot]", AuthorID: 49699333},
					{Org: "myorg", Repo: "myrepo", SHA: "c2", AuthorLogin: "human-dev", AuthorID: 1234567},
				},
			},
		}

		result := EvaluateCommit(botCommit, enrichment, []model.ExemptAuthor{{Login: "dependabot[bot]", ID: 49699333}}, nil, nil)
		assert.True(t, result.IsExemptAuthor)
		assert.True(t, result.IsCompliant, "independent review covers human contributor")
	})

	t.Run("bot-only PR — stays compliant via exempt", func(t *testing.T) {
		botCommit := squashCommit
		botCommit.AuthorLogin = "dependabot[bot]"
		botCommit.AuthorID = 49699333

		botPR := pr
		botPR.AuthorLogin = "dependabot[bot]"

		enrichment := model.EnrichmentResult{
			Commit: botCommit,
			PRs:    []model.PullRequest{botPR},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dependabot[bot]", AuthorID: 49699333},
				},
			},
		}

		result := EvaluateCommit(botCommit, enrichment, []model.ExemptAuthor{{Login: "dependabot[bot]", ID: 49699333}}, nil, nil)
		require.True(t, result.IsCompliant)
		assert.True(t, result.IsExemptAuthor)
		assert.Equal(t, []string{"exempt: configured author"}, result.Reasons)
	})

	t.Run("merge commit — PR commit authors still checked for self-approval", func(t *testing.T) {
		mergeCommit := squashCommit
		mergeCommit.ParentCount = 2
		mergeCommit.AuthorLogin = "merger"

		mergePR := pr
		mergePR.AuthorLogin = "dev-a"

		enrichment := model.EnrichmentResult{
			Commit:  mergeCommit,
			PRs:     []model.PullRequest{mergePR},
			Reviews: []model.Review{approvalFromDevB},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dev-a"},
					{Org: "myorg", Repo: "myrepo", SHA: "c2", AuthorLogin: "dev-b"},
				},
			},
		}

		result := EvaluateCommit(mergeCommit, enrichment, nil, nil, nil)
		assert.True(t, result.IsSelfApproved, "dev-b is both reviewer and PR commit author")
	})

	t.Run("CleanMerge — committer-as-reviewer is NOT self-approval", func(t *testing.T) {
		// GitHub's merge-button produces a 2-parent commit with a canned
		// message. The committer is whoever clicked merge; they did not
		// author any code in this commit (GitHub refuses the button on
		// conflicts). Reviewer == committer must stay clean.
		cleanMerge := squashCommit
		cleanMerge.ParentCount = 2
		cleanMerge.Message = "Merge pull request #10 from dev-a/feature"
		cleanMerge.AuthorLogin = "dev-b"
		cleanMerge.CommitterLogin = "web-flow"
		cleanMerge.IsVerified = true

		mergePR := pr
		mergePR.AuthorLogin = "dev-a"

		enrichment := model.EnrichmentResult{
			Commit:  cleanMerge,
			PRs:     []model.PullRequest{mergePR},
			Reviews: []model.Review{approvalFromDevB},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dev-a"},
				},
			},
		}

		result := EvaluateCommit(cleanMerge, enrichment, nil, nil, nil)
		assert.False(t, result.IsSelfApproved, "CleanMerge committer is not a code author")
		assert.True(t, result.IsCompliant)
	})

	t.Run("DirtyMerge — committer-as-reviewer IS self-approval", func(t *testing.T) {
		// A 2-parent commit with a non-auto message may include
		// conflict-resolution code authored by the committer. If the
		// committer also approves, that's self-approval.
		dirtyMerge := squashCommit
		dirtyMerge.ParentCount = 2
		dirtyMerge.Message = "Resolve conflicts in foo.go"
		dirtyMerge.AuthorLogin = "dev-b"
		dirtyMerge.CommitterLogin = "dev-b"

		mergePR := pr
		mergePR.AuthorLogin = "dev-a"

		enrichment := model.EnrichmentResult{
			Commit:  dirtyMerge,
			PRs:     []model.PullRequest{mergePR},
			Reviews: []model.Review{approvalFromDevB},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dev-a"},
				},
			},
		}

		result := EvaluateCommit(dirtyMerge, enrichment, nil, nil, nil)
		assert.True(t, result.IsSelfApproved, "DirtyMerge committer may have authored conflict-resolution code")
		assert.False(t, result.IsCompliant)
	})

	t.Run("spoofed CleanMerge message — committer-as-reviewer IS self-approval", func(t *testing.T) {
		// A malicious actor could craft a local merge commit with a message
		// that matches GitHub's `Merge pull request #N` format. Without
		// requiring committer == web-flow AND is_verified, the classifier
		// would trust the message and skip the committer check. This test
		// guards against that.
		spoofed := squashCommit
		spoofed.ParentCount = 2
		spoofed.Message = "Merge pull request #10 from dev-a/feature"
		spoofed.AuthorLogin = "dev-b"
		spoofed.CommitterLogin = "dev-b" // not web-flow
		spoofed.IsVerified = false

		mergePR := pr
		mergePR.AuthorLogin = "dev-a"

		enrichment := model.EnrichmentResult{
			Commit:  spoofed,
			PRs:     []model.PullRequest{mergePR},
			Reviews: []model.Review{approvalFromDevB},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dev-a"},
				},
			},
		}

		result := EvaluateCommit(spoofed, enrichment, nil, nil, nil)
		assert.True(t, result.IsSelfApproved, "spoofed merge message must not grant CleanMerge trust")
		assert.False(t, result.IsCompliant)
	})

	t.Run("web-flow committer but unverified — committer-as-reviewer IS self-approval", func(t *testing.T) {
		// Verification must also hold; otherwise the signal is forgeable.
		commit := squashCommit
		commit.ParentCount = 2
		commit.Message = "Merge pull request #10 from dev-a/feature"
		commit.AuthorLogin = "dev-b"
		commit.CommitterLogin = "web-flow"
		commit.IsVerified = false

		mergePR := pr
		mergePR.AuthorLogin = "dev-a"

		enrichment := model.EnrichmentResult{
			Commit:  commit,
			PRs:     []model.PullRequest{mergePR},
			Reviews: []model.Review{approvalFromDevB},
		}

		result := EvaluateCommit(commit, enrichment, nil, nil, nil)
		assert.True(t, result.IsSelfApproved, "unverified commit must not be trusted as CleanMerge")
	})

	t.Run("OctopusMerge — committer-as-reviewer IS self-approval", func(t *testing.T) {
		octopus := squashCommit
		octopus.ParentCount = 3
		octopus.Message = "Merge branches 'a', 'b', 'c'"
		octopus.AuthorLogin = "dev-b"
		octopus.CommitterLogin = "dev-b"

		mergePR := pr
		mergePR.AuthorLogin = "dev-a"

		enrichment := model.EnrichmentResult{
			Commit:  octopus,
			PRs:     []model.PullRequest{mergePR},
			Reviews: []model.Review{approvalFromDevB},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dev-a"},
				},
			},
		}

		result := EvaluateCommit(octopus, enrichment, nil, nil, nil)
		assert.True(t, result.IsSelfApproved, "OctopusMerge is not auto-trusted")
		assert.False(t, result.IsCompliant)
	})
}

// TestSelfApproval_EmptyAdminCommitByReviewer guards against a real-world
// false positive: a reviewer pushes an "Empty commit to rerun check"
// (or any zero-diff admin commit) onto the PR branch and then approves.
// Treating that as self-approval voids legitimate reviews. The fix lazy-
// fetches diff stats — only when an author's PR-branch contributions all
// look zero-stat — to disambiguate truly-empty admin commits from the
// listing-endpoint's missing-stats default.
//
// Modeled after https://github.com/linkedin-multiproduct/ad-targeting-spark/pull/1134
// where the reviewer's only PR-branch commit was "Empty commit to rerun check".
func TestSelfApproval_EmptyAdminCommitByReviewer(t *testing.T) {
	now := time.Now()

	commit := model.Commit{
		Org: "myorg", Repo: "myrepo", SHA: "merge1",
		AuthorLogin: "dev-a", CommittedAt: now,
		Message:     "feat: real change",
		ParentCount: 1, Additions: 50, Deletions: 5,
		Href: "https://github.com/myorg/myrepo/commit/merge1",
	}
	pr := model.PullRequest{
		Org: "myorg", Repo: "myrepo", Number: 10, HeadSHA: "head1",
		AuthorLogin: "dev-a", Merged: true,
		Href: "https://github.com/myorg/myrepo/pull/10",
	}
	approvalFromReviewer := model.Review{
		Org: "myorg", Repo: "myrepo", PRNumber: 10, ReviewID: 1,
		ReviewerLogin: "reviewer-r", State: "APPROVED",
		CommitID: "head1", SubmittedAt: now,
	}
	checks := []model.CheckRun{
		{Org: "myorg", Repo: "myrepo", CommitSHA: "head1", CheckRunID: 100, CheckName: "Owner Approval", Status: "completed", Conclusion: "success"},
	}
	required := []RequiredCheck{{Name: "Owner Approval", Conclusion: "success"}}

	t.Run("reviewer's only PR-branch commit is empty (fetchStats confirms 0,0) — compliant", func(t *testing.T) {
		var calls int
		fetchStats := func(_ StatsTrigger, _, _, sha string) (int, int, error) {
			calls++
			if sha == "empty1" {
				return 0, 0, nil
			}
			return 100, 0, nil
		}
		enrichment := model.EnrichmentResult{
			Commit: commit, PRs: []model.PullRequest{pr},
			Reviews: []model.Review{approvalFromReviewer}, CheckRuns: checks,
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "real1", AuthorLogin: "dev-a", Additions: 50, Deletions: 5},
					{Org: "myorg", Repo: "myrepo", SHA: "empty1", AuthorLogin: "reviewer-r"}, // Empty commit to rerun check
				},
			},
		}
		result := EvaluateCommit(commit, enrichment, nil, required, fetchStats)
		assert.False(t, result.IsSelfApproved, "reviewer's only contribution is empty admin commit")
		assert.True(t, result.IsCompliant)
		assert.Equal(t, 1, calls, "fetchStats invoked exactly once for the suspect commit")
	})

	t.Run("reviewer authored both empty and real commits — self-approved", func(t *testing.T) {
		// Short-circuits on the locally non-zero commit; never calls fetchStats.
		called := false
		fetchStats := func(_ StatsTrigger, _, _, _ string) (int, int, error) {
			called = true
			return 0, 0, nil
		}
		enrichment := model.EnrichmentResult{
			Commit: commit, PRs: []model.PullRequest{pr},
			Reviews: []model.Review{approvalFromReviewer}, CheckRuns: checks,
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "empty1", AuthorLogin: "reviewer-r"},
					{Org: "myorg", Repo: "myrepo", SHA: "real-by-reviewer", AuthorLogin: "reviewer-r", Additions: 30, Deletions: 1},
				},
			},
		}
		result := EvaluateCommit(commit, enrichment, nil, required, fetchStats)
		assert.True(t, result.IsSelfApproved, "reviewer also pushed a non-empty commit")
		assert.False(t, result.IsCompliant)
		assert.False(t, called, "non-zero local stats short-circuit before any API call")
	})

	t.Run("reviewer's lone commit looks empty locally but fetchStats reveals real diff — self-approved", func(t *testing.T) {
		fetchStats := func(_ StatsTrigger, _, _, sha string) (int, int, error) {
			if sha == "stats-hidden" {
				return 42, 7, nil // /pulls/N/commits omitted stats; reality is non-empty
			}
			return 0, 0, nil
		}
		enrichment := model.EnrichmentResult{
			Commit: commit, PRs: []model.PullRequest{pr},
			Reviews: []model.Review{approvalFromReviewer}, CheckRuns: checks,
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "stats-hidden", AuthorLogin: "reviewer-r"},
				},
			},
		}
		result := EvaluateCommit(commit, enrichment, nil, required, fetchStats)
		assert.True(t, result.IsSelfApproved, "fetchStats unmasked a real diff hidden by missing-stats default")
		assert.False(t, result.IsCompliant)
	})

	t.Run("fetchStats returns error — fail-safe to self-approval", func(t *testing.T) {
		fetchStats := func(_ StatsTrigger, _, _, _ string) (int, int, error) {
			return 0, 0, errors.New("transient API failure")
		}
		enrichment := model.EnrichmentResult{
			Commit: commit, PRs: []model.PullRequest{pr},
			Reviews: []model.Review{approvalFromReviewer}, CheckRuns: checks,
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "unknown", AuthorLogin: "reviewer-r"},
				},
			},
		}
		result := EvaluateCommit(commit, enrichment, nil, required, fetchStats)
		assert.True(t, result.IsSelfApproved, "API errors must not silently downgrade to compliant")
	})

	t.Run("nil fetchStats with zero-stat reviewer commit — preserves legacy conservative behaviour", func(t *testing.T) {
		enrichment := model.EnrichmentResult{
			Commit: commit, PRs: []model.PullRequest{pr},
			Reviews: []model.Review{approvalFromReviewer}, CheckRuns: checks,
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "empty1", AuthorLogin: "reviewer-r"},
				},
			},
		}
		result := EvaluateCommit(commit, enrichment, nil, required, nil)
		assert.True(t, result.IsSelfApproved, "without a fetcher we cannot distinguish empty from un-fetched")
	})
}

func TestEvaluateCommit_LazyStatsFetcher(t *testing.T) {
	// A commit with an approved PR must never invoke fetchStats: the audit
	// should short-circuit on the compliant path.
	t.Run("approved PR never invokes fetchStats", func(t *testing.T) {
		mergedAt := time.Now().Add(-time.Hour)
		commit := model.Commit{
			Org: "o", Repo: "r", SHA: "abc", AuthorLogin: "dev-a",
			// Additions/Deletions deliberately zero — pre-refactor this
			// would have flagged the commit "empty" and short-circuited
			// BEFORE the PR check, masking bugs. Now the PR check runs
			// first.
		}
		pr := model.PullRequest{
			Org: "o", Repo: "r", Number: 1, HeadSHA: "head",
			MergedAt: mergedAt,
		}
		enrichment := model.EnrichmentResult{
			PRs: []model.PullRequest{pr},
			Reviews: []model.Review{
				{Org: "o", Repo: "r", PRNumber: 1, CommitID: "head", ReviewerLogin: "reviewer", State: "APPROVED", SubmittedAt: mergedAt.Add(-time.Minute)},
			},
		}
		called := false
		result := EvaluateCommit(commit, enrichment, nil, nil, func(StatsTrigger, string, string, string) (int, int, error) {
			called = true
			return 1, 1, nil
		})
		assert.True(t, result.IsCompliant)
		assert.False(t, result.IsEmptyCommit, "approved PR should not be misclassified as empty")
		assert.False(t, called, "fetchStats must not be invoked when PR path succeeds")
	})

	// A non-compliant commit (no PR) with zero stats should invoke the
	// fetcher; a non-zero response flips the audit to "non-compliant" and
	// a zero/error response preserves the legacy "empty → compliant"
	// behaviour.
	t.Run("non-compliant commit invokes fetchStats and respects result", func(t *testing.T) {
		commit := model.Commit{Org: "o", Repo: "r", SHA: "abc", AuthorLogin: "dev-a"}
		enrichment := model.EnrichmentResult{}

		calls := 0
		result := EvaluateCommit(commit, enrichment, nil, nil, func(StatsTrigger, string, string, string) (int, int, error) {
			calls++
			return 42, 3, nil
		})
		assert.Equal(t, 1, calls, "fetchStats should run exactly once on fallback")
		assert.False(t, result.IsCompliant, "commit with real diff and no PR is non-compliant")
		assert.False(t, result.IsEmptyCommit)
		assert.Contains(t, strings.Join(result.Reasons, "|"), "no associated pull request")

		// Fetcher returns zero → empty-commit fallback fires.
		calls = 0
		result2 := EvaluateCommit(commit, enrichment, nil, nil, func(StatsTrigger, string, string, string) (int, int, error) {
			calls++
			return 0, 0, nil
		})
		assert.Equal(t, 1, calls)
		assert.True(t, result2.IsCompliant)
		assert.True(t, result2.IsEmptyCommit)
	})
}

func TestEvaluateCommit_RevertWaivers(t *testing.T) {
	// The only revert waiver is "clean revert": a diff-verified clean
	// revert is compliant standalone. Everything else — conflict-resolved
	// reverts, message-only, revert-of-revert, hand-crafted without
	// verification — falls through to the normal PR-approval rules. See
	// TODO.md for deferred variants (re-apply diff verification,
	// cross-commit chain).
	const revertedSHA = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	mergedAt := time.Now().Add(-time.Hour)
	revertPR := model.PullRequest{Org: "o", Repo: "r", Number: 7, HeadSHA: "head-7", MergedAt: mergedAt, Href: "https://example/pr/7"}
	baseCommit := model.Commit{Org: "o", Repo: "r", SHA: "revert-sha", AuthorLogin: "dev-a", Additions: 5, Deletions: 5}
	baseEnrichment := model.EnrichmentResult{PRs: []model.PullRequest{revertPR}}

	t.Run("diff-verified clean revert is compliant standalone", func(t *testing.T) {
		enrichment := baseEnrichment
		enrichment.IsCleanRevert = true
		enrichment.RevertVerification = "diff-verified"
		enrichment.RevertedSHA = revertedSHA
		result := EvaluateCommit(baseCommit, enrichment, nil, nil, nil)
		assert.True(t, result.IsCompliant)
		assert.True(t, result.IsCleanRevert, "is_clean_revert flag must survive so the Clean Reverts sheet still picks it up")
		require.Len(t, result.Reasons, 1)
		assert.Contains(t, result.Reasons[0], "clean revert of")
		assert.Contains(t, result.Reasons[0], "a1b2c3d4e5f6", "reason cites the reverted sha prefix")
	})

	t.Run("auto-revert without a reverted SHA is still compliant", func(t *testing.T) {
		enrichment := baseEnrichment
		enrichment.IsCleanRevert = true
		enrichment.RevertVerification = "message-only"
		enrichment.RevertedSHA = "" // trusted-by-construction auto-revert
		result := EvaluateCommit(baseCommit, enrichment, nil, nil, nil)
		assert.True(t, result.IsCompliant)
		assert.Contains(t, result.Reasons[0], "clean revert")
	})

	t.Run("message-only / diff-mismatch manual revert does NOT waive", func(t *testing.T) {
		// The revert prefix is in the message but we never confirmed the
		// diff is a pure inverse. Must fall through to PR-approval path
		// (which has no approval).
		commit := baseCommit
		commit.Message = `Revert "some change"` + "\n\nThis reverts commit abc123."
		enrichment := baseEnrichment
		enrichment.IsCleanRevert = false
		enrichment.RevertVerification = "diff-mismatch"
		enrichment.RevertedSHA = revertedSHA
		result := EvaluateCommit(commit, enrichment, nil, nil, nil)
		assert.False(t, result.IsCompliant)
	})

	t.Run("conflict-resolved GH-UI revert (web-flow + verified, diff mismatch) does NOT waive", func(t *testing.T) {
		// This is the scenario that previously waived under R2. Dropped
		// deliberately: conflict resolution introduces new code that
		// wasn't on master before, so a human should eyeball it.
		commit := baseCommit
		commit.Message = `Revert "feature X"` + "\n\nThis reverts commit a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2."
		commit.CommitterLogin = "web-flow"
		commit.IsVerified = true
		enrichment := baseEnrichment
		enrichment.IsCleanRevert = false
		enrichment.RevertVerification = "diff-mismatch"
		enrichment.RevertedSHA = revertedSHA
		result := EvaluateCommit(commit, enrichment, nil, nil, nil)
		assert.False(t, result.IsCompliant, "provenance alone is not enough — diff must verify")
	})

	t.Run("revert-of-revert (web-flow + verified) does NOT waive", func(t *testing.T) {
		// Re-applying previously-reverted code. Even via GH UI, the
		// decision to put it back should be reviewed.
		commit := baseCommit
		commit.Message = `Revert "Revert \"feature X\""`
		commit.CommitterLogin = "web-flow"
		commit.IsVerified = true
		result := EvaluateCommit(commit, baseEnrichment, nil, nil, nil)
		assert.False(t, result.IsCompliant)
	})

	t.Run("no cross-commit lookup: reverts do NOT depend on prior verdicts", func(t *testing.T) {
		// A clean revert is compliant regardless of the reverted
		// commit's audit state — this is the deferred-work boundary
		// documented in TODO.md.
		enrichment := baseEnrichment
		enrichment.IsCleanRevert = true
		enrichment.RevertVerification = "diff-verified"
		enrichment.RevertedSHA = revertedSHA
		result := EvaluateCommit(baseCommit, enrichment, nil, nil, nil)
		assert.True(t, result.IsCompliant)
	})
}

func TestEvaluateRevertCompliance(t *testing.T) {
	// Unit-level coverage of the waiver helper. Tests above exercise the
	// same logic end-to-end through EvaluateCommit; these are here so a
	// regression in the helper is pinpointed directly.
	revertMsg := `Revert "feature X"` + "\n\nThis reverts commit a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2."

	tests := []struct {
		name       string
		commit     model.Commit
		enrichment model.EnrichmentResult
		want       bool
		reasonHint string
	}{
		{
			name:       "fires on IsCleanRevert=true with reverted SHA",
			commit:     model.Commit{Message: revertMsg},
			enrichment: model.EnrichmentResult{IsCleanRevert: true, RevertedSHA: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"},
			want:       true,
			reasonHint: "clean revert of",
		},
		{
			name:       "fires on IsCleanRevert=true without reverted SHA (auto-revert)",
			commit:     model.Commit{Message: "Automatic revert of abc..def"},
			enrichment: model.EnrichmentResult{IsCleanRevert: true},
			want:       true,
			reasonHint: "clean revert",
		},
		{
			name:       "no waiver: revert message + web-flow + verified but diff-mismatch",
			commit:     model.Commit{Message: revertMsg, CommitterLogin: "web-flow", IsVerified: true},
			enrichment: model.EnrichmentResult{IsCleanRevert: false, RevertVerification: "diff-mismatch"},
			want:       false,
		},
		{
			name:       "no waiver: message-only ManualRevert",
			commit:     model.Commit{Message: revertMsg},
			enrichment: model.EnrichmentResult{IsCleanRevert: false, RevertVerification: "message-only"},
			want:       false,
		},
		{
			name:       "no waiver: non-revert commit",
			commit:     model.Commit{Message: "Add feature Y"},
			enrichment: model.EnrichmentResult{IsCleanRevert: false},
			want:       false,
		},
		{
			name:       "no waiver: revert-of-revert (never classified clean)",
			commit:     model.Commit{Message: `Revert "Revert \"feature X\""`, CommitterLogin: "web-flow", IsVerified: true},
			enrichment: model.EnrichmentResult{IsCleanRevert: false},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := evaluateRevertCompliance(tt.commit, tt.enrichment)
			assert.Equal(t, tt.want, got)
			if tt.reasonHint != "" {
				assert.Contains(t, reason, tt.reasonHint)
			}
		})
	}
}

// TestLatestReviewStatesOnFinal_CaseSensitivity verifies that the per-reviewer
// latest-state map treats login casing as equivalent. GitHub normalizes logins
// but cached data or renamed accounts may introduce casing variance.
func TestLatestReviewStatesOnFinal_CaseSensitivity(t *testing.T) {
	pr := model.PullRequest{
		Number:  1,
		HeadSHA: "head",
		MergedAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	t1 := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 5, 1, 11, 0, 0, 0, time.UTC)

	t.Run("same reviewer different casing collapses to one entry", func(t *testing.T) {
		reviews := []model.Review{
			{PRNumber: 1, CommitID: "head", ReviewerLogin: "Alice", ReviewID: 1, State: "APPROVED", SubmittedAt: t1},
			{PRNumber: 1, CommitID: "head", ReviewerLogin: "alice", ReviewID: 2, State: "CHANGES_REQUESTED", SubmittedAt: t2},
		}
		latest, _ := latestReviewStatesOnFinal(reviews, pr)
		require.Len(t, latest, 1, "same reviewer with different casing must collapse to one map entry")
		for _, r := range latest {
			assert.Equal(t, "CHANGES_REQUESTED", r.State, "later CHANGES_REQUESTED must clobber earlier APPROVED")
		}
	})

	t.Run("COMMENTED with different casing does not clobber APPROVED", func(t *testing.T) {
		reviews := []model.Review{
			{PRNumber: 1, CommitID: "head", ReviewerLogin: "Bob", ReviewID: 1, State: "APPROVED", SubmittedAt: t1},
			{PRNumber: 1, CommitID: "head", ReviewerLogin: "bob", ReviewID: 2, State: "COMMENTED", SubmittedAt: t2},
		}
		latest, _ := latestReviewStatesOnFinal(reviews, pr)
		require.Len(t, latest, 1)
		for _, r := range latest {
			assert.Equal(t, "APPROVED", r.State, "COMMENTED must not clobber APPROVED even with casing mismatch")
		}
	})
}

// TestLatestReviewStatesOnFinal_SameTimestampTiebreak verifies that when two
// reviews from the same reviewer have identical SubmittedAt, the one with the
// higher ReviewID wins (later creation). Without this, slice order determines
// the outcome — an observable correctness hazard.
func TestLatestReviewStatesOnFinal_SameTimestampTiebreak(t *testing.T) {
	pr := model.PullRequest{
		Number:  1,
		HeadSHA: "head",
		MergedAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	sameTime := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)

	t.Run("higher ReviewID wins when timestamps are equal", func(t *testing.T) {
		reviews := []model.Review{
			{PRNumber: 1, CommitID: "head", ReviewerLogin: "reviewer", ReviewID: 100, State: "APPROVED", SubmittedAt: sameTime},
			{PRNumber: 1, CommitID: "head", ReviewerLogin: "reviewer", ReviewID: 200, State: "CHANGES_REQUESTED", SubmittedAt: sameTime},
		}
		latest, _ := latestReviewStatesOnFinal(reviews, pr)
		require.Len(t, latest, 1)
		assert.Equal(t, "CHANGES_REQUESTED", latest["reviewer"].State,
			"higher ReviewID must win as tiebreaker when timestamps are equal")
	})

	t.Run("order independence with same timestamp", func(t *testing.T) {
		// Reversed slice order — must produce the same result.
		reviews := []model.Review{
			{PRNumber: 1, CommitID: "head", ReviewerLogin: "reviewer", ReviewID: 200, State: "CHANGES_REQUESTED", SubmittedAt: sameTime},
			{PRNumber: 1, CommitID: "head", ReviewerLogin: "reviewer", ReviewID: 100, State: "APPROVED", SubmittedAt: sameTime},
		}
		latest, _ := latestReviewStatesOnFinal(reviews, pr)
		require.Len(t, latest, 1)
		assert.Equal(t, "CHANGES_REQUESTED", latest["reviewer"].State,
			"result must be order-independent — higher ReviewID wins regardless of slice position")
	})
}

// TestEvaluateCommit_ReviewerCasingBug is an integration test proving that
// reviewer login casing differences do not create a false-compliant verdict.
func TestEvaluateCommit_ReviewerCasingBug(t *testing.T) {
	f := newAuditBaseline()
	cases := []evalCase{
		{
			name:   "APPROVED then CHANGES_REQUESTED from same reviewer (different casing) → non-compliant",
			commit: f.commit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{f.pr},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "Reviewer1", State: "APPROVED", CommitID: "head123", SubmittedAt: f.now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED", CommitID: "head123", SubmittedAt: f.now},
				},
				CheckRuns: []model.CheckRun{f.ownerApprovalCheck},
			},
			requiredChecks: f.requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
	}
	runEvalCases(t, cases)
}
