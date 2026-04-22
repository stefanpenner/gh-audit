package sync

import (
	"strings"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		name                 string
		commit               model.Commit
		enrichment           model.EnrichmentResult
		exemptAuthors        []string
		requiredChecks       []RequiredCheck
		wantCompliant        bool
		wantBot              bool
		wantExempt           bool
		wantEmpty            bool
		wantHasPR            bool
		wantSelfApproved     bool
		wantStaleApproval    bool
		wantPostMergeConcern bool
		wantReasons          []string
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
			requiredChecks:    requiredChecks,
			wantCompliant:     false,
			wantHasPR:         true,
			wantStaleApproval: true,
			wantReasons:       []string{"approval is stale \u2014 not on final commit (PR #42)"},
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
			requiredChecks:    requiredChecks,
			wantCompliant:     false,
			wantHasPR:         true,
			wantStaleApproval: true,
			wantReasons:       []string{"approval is stale \u2014 not on final commit (PR #42)"},
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
			requiredChecks:    requiredChecks,
			wantCompliant:     false,
			wantHasPR:         true,
			wantStaleApproval: true,
			wantReasons:       []string{"approval is stale \u2014 not on final commit (PR #42)"},
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
			name:   "APPROVED then COMMENTED on final commit is still compliant",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "reviewer1", State: "APPROVED", CommitID: "head123", SubmittedAt: now.Add(-time.Hour)},
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2, ReviewerLogin: "reviewer1", State: "COMMENTED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
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
			name:   "only self-approval on old SHA is not flagged as stale",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "developer", State: "APPROVED", CommitID: "old-sha", SubmittedAt: now.Add(-time.Hour)},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
			wantCompliant:  false,
			wantHasPR:      true,
			wantReasons:    []string{"no approval on final commit (PR #42)"},
		},
		{
			name: "merge commit author is merger not code author — not self-approval",
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
					{Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 1, ReviewerLogin: "merger", State: "APPROVED", CommitID: "head123", SubmittedAt: now},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks: requiredChecks,
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
		// --- Post-merge concern tracking ---
		//
		// Point-in-time compliance: reviews submitted after pr.MergedAt are
		// ignored for the latest-state-wins decision, and post-merge
		// CHANGES_REQUESTED / DISMISSED reviews set HasPostMergeConcern.
		{
			name:   "approved before merge, CHANGES_REQUESTED after merge → compliant + post-merge concern",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					approvedReview, // submitted at `now`, matches pr.MergedAt — included (Before is strict)
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 2,
						ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED",
						CommitID: "head123", SubmittedAt: now.Add(time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:       requiredChecks,
			wantCompliant:        true,
			wantHasPR:            true,
			wantPostMergeConcern: true,
			wantReasons:          []string{"compliant"},
		},
		{
			name:   "approved before merge, DISMISSED after merge → compliant + post-merge concern",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					approvedReview,
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 3,
						ReviewerLogin: "reviewer1", State: "DISMISSED",
						CommitID: "head123", SubmittedAt: now.Add(2 * time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:       requiredChecks,
			wantCompliant:        true,
			wantHasPR:            true,
			wantPostMergeConcern: true,
			wantReasons:          []string{"compliant"},
		},
		{
			name:   "approved before merge, COMMENTED after merge → compliant, no concern",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{basePR},
				Reviews: []model.Review{
					approvedReview,
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 4,
						ReviewerLogin: "reviewer2", State: "COMMENTED",
						CommitID: "head123", SubmittedAt: now.Add(time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:       requiredChecks,
			wantCompliant:        true,
			wantHasPR:            true,
			wantPostMergeConcern: false,
			wantReasons:          []string{"compliant"},
		},
		{
			name:   "open PR (no MergedAt) with later CHANGES_REQUESTED → no concern, non-compliant",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{func() model.PullRequest {
					p := basePR
					p.Merged = false
					p.MergedAt = time.Time{}
					return p
				}()},
				Reviews: []model.Review{
					approvedReview,
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 5,
						ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED",
						CommitID: "head123", SubmittedAt: now.Add(time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:       requiredChecks,
			wantCompliant:        false,
			wantHasPR:            true,
			wantPostMergeConcern: false,
			wantReasons:          []string{"no approval on final commit (PR #42)"},
		},
		// --- Clean-revert wire-up ---
		//
		// Cheap pass-through: the enricher sets IsCleanRevert / RevertVerification /
		// RevertedSHA on EnrichmentResult; EvaluateCommit copies those fields onto
		// AuditResult regardless of compliance path. Purely informational.
		{
			name: "enrichment marks auto-revert clean — result carries the flag",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{basePR},
				Reviews:   []model.Review{approvedReview},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},

				IsCleanRevert:      true,
				RevertVerification: "message-only",
				RevertedSHA:        "abcdef1234567890abcdef1234567890abcdef12",
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name: "enrichment marks diff-verified manual revert — result carries the flag",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{basePR},
				Reviews:   []model.Review{approvedReview},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},

				IsCleanRevert:      true,
				RevertVerification: "diff-verified",
				RevertedSHA:        "1234567890abcdef1234567890abcdef12345678",
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name: "enrichment marks diff-mismatch — IsCleanRevert stays false but verification recorded",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs:       []model.PullRequest{basePR},
				Reviews:   []model.Review{approvedReview},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},

				IsCleanRevert:      false,
				RevertVerification: "diff-mismatch",
				RevertedSHA:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
			requiredChecks: requiredChecks,
			wantCompliant:  true,
			wantHasPR:      true,
			wantReasons:    []string{"compliant"},
		},
		{
			name:   "reviewer flipped before merge stays non-compliant (unchanged behaviour)",
			commit: baseCommit,
			enrichment: model.EnrichmentResult{
				PRs: []model.PullRequest{func() model.PullRequest {
					p := basePR
					p.MergedAt = now.Add(5 * time.Minute)
					return p
				}()},
				Reviews: []model.Review{
					approvedReview, // APPROVED at `now`
					{
						Org: "myorg", Repo: "myrepo", PRNumber: 42, ReviewID: 6,
						ReviewerLogin: "reviewer1", State: "CHANGES_REQUESTED",
						CommitID: "head123", SubmittedAt: now.Add(2 * time.Minute),
					},
				},
				CheckRuns: []model.CheckRun{ownerApprovalCheck},
			},
			requiredChecks:       requiredChecks,
			wantCompliant:        false,
			wantHasPR:            true,
			wantPostMergeConcern: false,
			wantReasons:          []string{"no approval on final commit (PR #42)"},
		},
	}

	for _, tt := range tests {
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
			// Clean-revert pass-through: whatever the enrichment set must land on the result.
			assert.Equal(t, tt.enrichment.IsCleanRevert, result.IsCleanRevert, "IsCleanRevert")
			assert.Equal(t, tt.enrichment.RevertVerification, result.RevertVerification, "RevertVerification")
			assert.Equal(t, tt.enrichment.RevertedSHA, result.RevertedSHA, "RevertedSHA")
			if tt.wantReasons != nil {
				require.Equal(t, tt.wantReasons, result.Reasons, "Reasons")
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
		Org:            "myorg",
		Repo:           "myrepo",
		PRNumber:       10,
		ReviewID:       1,
		ReviewerLogin:  "dev-b",
		State:          "APPROVED",
		CommitID:       "head1",
		SubmittedAt:    now,
	}

	approvalFromIndependent := model.Review{
		Org:            "myorg",
		Repo:           "myrepo",
		PRNumber:       10,
		ReviewID:       2,
		ReviewerLogin:  "independent-reviewer",
		State:          "APPROVED",
		CommitID:       "head1",
		SubmittedAt:    now,
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

		botPR := pr
		botPR.AuthorLogin = "dependabot[bot]"

		enrichment := model.EnrichmentResult{
			Commit: botCommit,
			PRs:    []model.PullRequest{botPR},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dependabot[bot]"},
					{Org: "myorg", Repo: "myrepo", SHA: "c2", AuthorLogin: "human-dev"},
				},
			},
		}

		result := EvaluateCommit(botCommit, enrichment, []string{"dependabot[bot]"}, nil, nil)
		assert.True(t, result.IsExemptAuthor, "commit author is exempt")
		assert.False(t, result.IsCompliant, "non-exempt contributor needs review")
	})

	t.Run("exempt bot + human contributor, reviewed — compliant", func(t *testing.T) {
		botCommit := squashCommit
		botCommit.AuthorLogin = "dependabot[bot]"

		botPR := pr
		botPR.AuthorLogin = "dependabot[bot]"

		enrichment := model.EnrichmentResult{
			Commit:  botCommit,
			PRs:     []model.PullRequest{botPR},
			Reviews: []model.Review{approvalFromIndependent},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dependabot[bot]"},
					{Org: "myorg", Repo: "myrepo", SHA: "c2", AuthorLogin: "human-dev"},
				},
			},
		}

		result := EvaluateCommit(botCommit, enrichment, []string{"dependabot[bot]"}, nil, nil)
		assert.True(t, result.IsExemptAuthor)
		assert.True(t, result.IsCompliant, "independent review covers human contributor")
	})

	t.Run("bot-only PR — stays compliant via exempt", func(t *testing.T) {
		botCommit := squashCommit
		botCommit.AuthorLogin = "dependabot[bot]"

		botPR := pr
		botPR.AuthorLogin = "dependabot[bot]"

		enrichment := model.EnrichmentResult{
			Commit: botCommit,
			PRs:    []model.PullRequest{botPR},
			PRBranchCommits: map[int][]model.Commit{
				10: {
					{Org: "myorg", Repo: "myrepo", SHA: "c1", AuthorLogin: "dependabot[bot]"},
				},
			},
		}

		result := EvaluateCommit(botCommit, enrichment, []string{"dependabot[bot]"}, nil, nil)
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
		result := EvaluateCommit(commit, enrichment, nil, nil, func(string, string, string) (int, int, error) {
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
		result := EvaluateCommit(commit, enrichment, nil, nil, func(string, string, string) (int, int, error) {
			calls++
			return 42, 3, nil
		})
		assert.Equal(t, 1, calls, "fetchStats should run exactly once on fallback")
		assert.False(t, result.IsCompliant, "commit with real diff and no PR is non-compliant")
		assert.False(t, result.IsEmptyCommit)
		assert.Contains(t, strings.Join(result.Reasons, "|"), "no associated pull request")

		// Fetcher returns zero → empty-commit fallback fires.
		calls = 0
		result2 := EvaluateCommit(commit, enrichment, nil, nil, func(string, string, string) (int, int, error) {
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
