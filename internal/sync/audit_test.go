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
		name             string
		commit           model.Commit
		enrichment       model.EnrichmentResult
		exemptAuthors    []string
		requiredChecks   []RequiredCheck
		wantCompliant    bool
		wantBot          bool
		wantExempt       bool
		wantEmpty        bool
		wantHasPR        bool
		wantSelfApproved bool
		wantReasons      []string
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
			name:          "exempt author is compliant",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "dependabot[bot]", Additions: 5, Deletions: 3},
			enrichment:    model.EnrichmentResult{},
			exemptAuthors: []string{"dependabot[bot]", "renovate[bot]"},
			wantCompliant: true,
			wantBot:       true,
			wantExempt:    true,
			wantReasons:   []string{"exempt: configured author"},
		},
		{
			name:          "bot not in exempt list is not exempt",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "some-ci[bot]", Additions: 5, Deletions: 3},
			enrichment:    model.EnrichmentResult{},
			exemptAuthors: []string{"dependabot[bot]"},
			wantCompliant: false,
			wantBot:       true,
			wantExempt:    false,
			wantReasons:   []string{"no associated pull request"},
		},
		{
			name:          "non-bot exempt author is exempt but not bot",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "service-account", Additions: 5, Deletions: 3},
			enrichment:    model.EnrichmentResult{},
			exemptAuthors: []string{"service-account"},
			wantCompliant: true,
			wantBot:       false,
			wantExempt:    true,
			wantReasons:   []string{"exempt: configured author"},
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
			name:          "commit with additions is not empty",
			commit:        model.Commit{Org: "myorg", Repo: "myrepo", SHA: "abc123", AuthorLogin: "developer", Additions: 42, Deletions: 0},
			enrichment:    model.EnrichmentResult{},
			wantCompliant: false,
			wantEmpty:     false,
			wantReasons:   []string{"no associated pull request"},
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
		{
			name:   "PR author == reviewer is self-approval",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "developer", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:   requiredChecks,
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
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "coder", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:   requiredChecks,
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
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "codev", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:   requiredChecks,
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
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "deployer", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:   requiredChecks,
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
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "web-flow", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "self-approval exists but another non-self approval also exists is compliant",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "developer", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "independent-reviewer", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "stale approval after force-push",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-force-pushed-sha", SubmittedAt: now.Add(-time.Hour)},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "stale approval from one reviewer fresh approval from another",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-force-pushed-sha", SubmittedAt: now.Add(-2 * time.Hour)},
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
			name:   "same reviewer old APPROVED then CHANGES_REQUESTED on final",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-sha", SubmittedAt: now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "re-approval after force-push same reviewer",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "old-force-pushed-sha", SubmittedAt: now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "DISMISSED review on final commit is non-compliant",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "DISMISSED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "APPROVED then DISMISSED on final commit is non-compliant",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "head123", SubmittedAt: now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "DISMISSED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name:   "mixed states on final CHANGES_REQUESTED and APPROVED from different reviewers",
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
			name:   "multiple required checks all pass",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{basePR},
				Reviews: []model.Review{approvedReview},
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
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{basePR},
				Reviews: []model.Review{approvedReview},
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
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:     []model.PullRequest{basePR},
				Reviews: []model.Review{approvedReview},
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
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "codev", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "external-reviewer", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
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
			if result.IsExemptAuthor != tt.wantExempt {
				t.Errorf("IsExemptAuthor = %v, want %v", result.IsExemptAuthor, tt.wantExempt)
			}
			if result.IsEmptyCommit != tt.wantEmpty {
				t.Errorf("IsEmptyCommit = %v, want %v", result.IsEmptyCommit, tt.wantEmpty)
			}
			if result.HasPR != tt.wantHasPR {
				t.Errorf("HasPR = %v, want %v", result.HasPR, tt.wantHasPR)
			}
			if result.IsSelfApproved != tt.wantSelfApproved {
				t.Errorf("IsSelfApproved = %v, want %v", result.IsSelfApproved, tt.wantSelfApproved)
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
			want:           "success",
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
			if got != tt.want {
				t.Errorf("evaluateRequiredChecks() = %q, want %q", got, tt.want)
			}
		})
	}
}
