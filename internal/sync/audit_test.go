package sync

import (
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

func TestEvaluateCommit(t *testing.T) {
	now := time.Now()

	baseCommit := model.Commit{
		Org:         "myorg",
		Repo:        "myrepo",
		SHA:         "abc123",
		AuthorLogin: "developer",
		CommittedAt: now,
		Message:     "feat: add feature",
		Additions:   10,
		Deletions:   5,
		Href:        "https://github.com/myorg/myrepo/commit/abc123",
	}

	basePR := model.PullRequest{
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
	}

	approvedReview := model.Review{
		Org:           "myorg",
		Repo:          "myrepo",
		PRNumber:      42,
		ReviewID:      1,
		ReviewerLogin: "reviewer1",
		State:         "APPROVED",
		CommitID:      "head123",
		SubmittedAt:   now,
	}

	ownerApprovalCheck := model.CheckRun{
		Org:        "myorg",
		Repo:       "myrepo",
		CommitSHA:  "head123",
		CheckRunID: 100,
		CheckName:  "Owner Approval",
		Status:     "completed",
		Conclusion: "success",
	}

	requiredChecks := []RequiredCheck{
		{Name: "Owner Approval", Conclusion: "success"},
	}

	tests := []struct {
		name           string
		commit         model.Commit
		enrichment     model.EnrichmentResult
		exemptAuthors  []string
		requiredChecks []RequiredCheck
		wantCompliant  bool
		wantBot        bool
		wantEmpty      bool
		wantHasPR      bool
		wantReasons    []string
	}{
		{
			name:   "normal compliant commit",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{basePR},
				Reviews:   []model.Review{approvedReview},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:          "bot author is exempt",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "dependabot", Additions: 5, Deletions: 3},
			enrichment:    model.EnrichmentResult{},
			exemptAuthors: []string{"dependabot", "renovate"},
			wantCompliant: true,
			wantBot:       true,
			wantReasons:   []string{"exempt: bot author"},
		},
		{
			name:          "empty commit is exempt",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "developer", Additions: 0, Deletions: 0},
			enrichment:    model.EnrichmentResult{},
			wantCompliant: true,
			wantEmpty:     true,
			wantReasons:   []string{"empty commit"},
		},
		{
			name:          "no PR is non-compliant",
			commit:        baseCommit,
			enrichment:    model.EnrichmentResult{},
			wantCompliant: false,
			wantHasPR:     false,
			wantReasons:   []string{"no associated pull request"},
		},
		{
			name:   "PR exists but no reviews",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{basePR},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "review on non-final commit",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-commit-sha"},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "approved on final but Owner Approval missing",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{basePR},
				Reviews: []model.Review{approvedReview},
				// no check runs
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"Owner Approval check missing/failed (PR #42)"},
		},
		{
			name:   "approved on final but Owner Approval failed",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{basePR},
				Reviews: []model.Review{approvedReview},
				CheckRuns: []model.CheckRun{
					{Org: "myorg", Repo: "myrepo", CommitSHA: "head123", CheckRunID: 100, CheckName: "Owner Approval", Status: "completed", Conclusion: "failure"},
				},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"Owner Approval check missing/failed (PR #42)"},
		},
		{
			name:   "CHANGES_REQUESTED then APPROVED on final commit",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED", CommitID: "head123", SubmittedAt: now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer2", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "multiple PRs first non-compliant second compliant",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 41, HeadSHA: "head-old", Href: "https://github.com/myorg/myrepo/pull/41"},
					basePR,
				},
				Reviews:   []model.Review{approvedReview},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "multiple PRs all non-compliant",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{
					{Org: "myorg", Repo: "myrepo", Number: 41, HeadSHA: "head-old", Href: "https://github.com/myorg/myrepo/pull/41"},
					{Org: "myorg", Repo: "myrepo", Number: 42, HeadSHA: "head123", Href: "https://github.com/myorg/myrepo/pull/42"},
				},
				// No reviews, no check runs
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
		},
		{
			name:   "merge commit treated normally",
			commit: model.Commit{Org: "myorg", Repo: "myrepo", SHA: "merge123", AuthorLogin: "developer", ParentCount: 2, Additions: 10, Deletions: 5, Href: "https://github.com/myorg/myrepo/commit/merge123"},
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{basePR},
				Reviews:   []model.Review{approvedReview},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "no required checks means Owner Approval not needed",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{basePR},
				Reviews: []model.Review{approvedReview},
			},
			requiredChecks: nil, // no required checks
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EvaluateCommit(tt.commit, tt.enrichment, tt.exemptAuthors, tt.requiredChecks)

			if result.IsCompliant != tt.wantCompliant {
				t.Errorf("IsCompliant = %v, want %v (reasons: %v)", result.IsCompliant, tt.wantCompliant, result.Reasons)
			}
			if result.IsBot != tt.wantBot {
				t.Errorf("IsBot = %v, want %v", result.IsBot, tt.wantBot)
			}
			if result.IsEmptyCommit != tt.wantEmpty {
				t.Errorf("IsEmptyCommit = %v, want %v", result.IsEmptyCommit, tt.wantEmpty)
			}
			if result.HasPR != tt.wantHasPR {
				t.Errorf("HasPR = %v, want %v", result.HasPR, tt.wantHasPR)
			}
			if tt.wantReasons != nil {
				if len(result.Reasons) != len(tt.wantReasons) {
					t.Errorf("Reasons = %v, want %v", result.Reasons, tt.wantReasons)
				} else {
					for i, r := range tt.wantReasons {
						if result.Reasons[i] != r {
							t.Errorf("Reasons[%d] = %q, want %q", i, result.Reasons[i], r)
						}
					}
				}
			}
		})
	}
}
