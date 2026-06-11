package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stefanpenner/gh-audit/internal/model"
)

func benchCommits(n int) []model.Commit {
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	out := make([]model.Commit, n)
	for i := range out {
		out[i] = model.Commit{
			Org: "o", Repo: "r", SHA: fmt.Sprintf("sha%06d", i),
			AuthorLogin: "dev", AuthorID: 1, AuthorEmail: "dev@example.com",
			CommitterLogin: "web-flow", CommittedAt: at.Add(time.Duration(i) * time.Minute),
			Message:     fmt.Sprintf("feat: change %d (#%d)", i, i),
			ParentCount: 1, Additions: 10, Deletions: 2,
			Href: fmt.Sprintf("https://github.com/o/r/commit/sha%06d", i),
		}
	}
	return out
}

// Re-upserting the same rows exercises the merge path against existing
// rows plus the commit-detail preservation pre-merge UPDATE.
func BenchmarkUpsertCommits_1kReplace(b *testing.B) {
	db, err := OpenMemory()
	require.NoError(b, err)
	defer db.Close()
	ctx := context.Background()
	commits := benchCommits(1000)
	require.NoError(b, db.UpsertCommits(ctx, commits))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.UpsertCommits(ctx, commits); err != nil {
			b.Fatal(err)
		}
	}
}

// audit_results rows carry LIST columns, so re-upserts go through the
// DELETE+INSERT fallback — the most expensive merge shape.
func BenchmarkUpsertAuditResults_1kReplace(b *testing.B) {
	db, err := OpenMemory()
	require.NoError(b, err)
	defer db.Close()
	ctx := context.Background()
	results := make([]model.AuditResult, 1000)
	for i := range results {
		results[i] = model.AuditResult{
			Org: "o", Repo: "r", SHA: fmt.Sprintf("sha%06d", i),
			IsCompliant: i%3 == 0, HasPR: true, PRNumber: i,
			Reasons:        []string{"no approval on final commit (PR #1)"},
			ApproverLogins: []string{"rev1"},
			AuditedAt:      time.Now(),
		}
	}
	require.NoError(b, db.UpsertAuditResults(ctx, results))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.UpsertAuditResults(ctx, results); err != nil {
			b.Fatal(err)
		}
	}
}

// The batched link writer vs the per-row variant it replaced in the
// enrichment hot path — documents the win and guards against regressing
// back to per-row writes.
func BenchmarkUpsertCommitPRLinks_500Batched(b *testing.B) {
	db, err := OpenMemory()
	require.NoError(b, err)
	defer db.Close()
	ctx := context.Background()
	links := make([]CommitPRLink, 500)
	for i := range links {
		links[i] = CommitPRLink{SHA: fmt.Sprintf("sha%06d", i), PRNumber: i % 50}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.UpsertCommitPRLinks(ctx, "o", "r", links); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUpsertCommitPRs_500PerRow(b *testing.B) {
	db, err := OpenMemory()
	require.NoError(b, err)
	defer db.Close()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 500; j++ {
			if err := db.UpsertCommitPRs(ctx, "o", "r", fmt.Sprintf("sha%06d", j), []int{j % 50}); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// The enrichment path's per-commit lookup: must stay sha-scoped (the
// whole-repo co_authors scan here used to make sweeps quadratic).
func BenchmarkGetCommitsBySHA_SingleAmong10k(b *testing.B) {
	db, err := OpenMemory()
	require.NoError(b, err)
	defer db.Close()
	ctx := context.Background()
	commits := benchCommits(10000)
	for i := range commits {
		commits[i].CoAuthors = []model.CoAuthor{{Name: "Co", Email: fmt.Sprintf("co%d@x.com", i)}}
	}
	require.NoError(b, db.UpsertCommits(ctx, commits))
	require.NoError(b, db.UpsertCoAuthors(ctx, commits))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := db.GetCommitsBySHA(ctx, "o", "r", []string{"sha005000"})
		if err != nil || len(got) != 1 {
			b.Fatalf("got %d, err %v", len(got), err)
		}
	}
}
