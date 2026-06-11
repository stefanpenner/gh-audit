package sync

import (
	"fmt"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// benchEnrichment builds a realistic enrichment: one merged PR with
// reviews on the head SHA, a handful of branch commits, and check runs.
func benchEnrichment(prs, reviewsPerPR, branchCommits int) (model.Commit, model.EnrichmentResult) {
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	commit := model.Commit{
		Org: "o", Repo: "r", SHA: "head-0",
		AuthorID: 1, AuthorLogin: "author", CommitterLogin: "web-flow",
		Additions: 120, Deletions: 30, ParentCount: 1,
		Message: "feat: change (#1)", CommittedAt: at,
	}
	e := model.EnrichmentResult{Commit: commit, PRBranchCommits: map[int][]model.Commit{}}
	for p := 0; p < prs; p++ {
		head := fmt.Sprintf("head-%d", p)
		pr := model.PullRequest{
			Org: "o", Repo: "r", Number: p + 1, Merged: true,
			HeadSHA: head, AuthorID: 1, MergedAt: at.Add(time.Hour),
		}
		e.PRs = append(e.PRs, pr)
		for i := 0; i < reviewsPerPR; i++ {
			state := "COMMENTED"
			if i == reviewsPerPR-1 {
				state = "APPROVED"
			}
			e.Reviews = append(e.Reviews, model.Review{
				PRNumber: pr.Number, ReviewID: int64(p*1000 + i),
				ReviewerID: int64(100 + i), ReviewerLogin: fmt.Sprintf("rev%d", i),
				State: state, CommitID: head, SubmittedAt: at.Add(time.Duration(i) * time.Minute),
			})
		}
		for i := 0; i < branchCommits; i++ {
			e.PRBranchCommits[pr.Number] = append(e.PRBranchCommits[pr.Number], model.Commit{
				Org: "o", Repo: "r", SHA: fmt.Sprintf("bc-%d-%d", p, i),
				AuthorID: int64(200 + i), AuthorLogin: fmt.Sprintf("dev%d", i),
				Additions: 10, Deletions: 2, CommittedAt: at,
			})
		}
		e.CheckRuns = append(e.CheckRuns, model.CheckRun{
			CommitSHA: head, CheckName: "Owner Approval", Conclusion: "success",
			CheckRunID: int64(p), CompletedAt: at,
		})
	}
	return commit, e
}

func BenchmarkEvaluateCommit_CompliantSinglePR(b *testing.B) {
	commit, e := benchEnrichment(1, 3, 5)
	checks := []RequiredCheck{{Name: "Owner Approval", Conclusion: "success"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := EvaluateCommit(commit, e, nil, checks, nil)
		if !r.IsCompliant {
			b.Fatal("expected compliant")
		}
	}
}

func BenchmarkEvaluateCommit_MultiPRWorstCase(b *testing.B) {
	// 5 PRs, none compliant (reviews are self-approvals) — exercises the
	// full §7 tournament plus §5 contributor filtering on every PR.
	commit, e := benchEnrichment(5, 4, 20)
	for i := range e.Reviews {
		e.Reviews[i].ReviewerID = 1 // PR author — every approval is self
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := EvaluateCommit(commit, e, nil, nil, nil)
		if r.IsCompliant {
			b.Fatal("expected non-compliant")
		}
	}
}

func BenchmarkLatestReviewStatesOnFinal_200Reviews(b *testing.B) {
	_, e := benchEnrichment(1, 200, 0)
	pr := e.PRs[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		latestReviewStatesOnFinal(e.Reviews, pr)
	}
}

func BenchmarkComputeAnnotations(b *testing.B) {
	commit := model.Commit{
		Message: "chore(deps): bump lodash from 4.17.20 to 4.17.21 (#9)\n\nSigned-off-by: dependabot[bot]",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ComputeAnnotations(commit, model.EnrichmentResult{})
	}
}
