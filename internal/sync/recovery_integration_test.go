package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stefanpenner/gh-audit/internal/db"
	"github.com/stefanpenner/gh-audit/internal/model"
)

// The recovery contract: a sync interrupted at ANY point must leave the
// database in a state from which the next plain run converges to the same
// verdicts an uninterrupted run produces — no lost commits, no permanently
// unaudited backlog, no duplicated or contradictory audit rows. These
// tests drive the REAL pipeline against a REAL DuckDB (mock GitHub
// source/enricher) and break it on purpose.

// recoverySource serves a fixed commit list via the date-window path
// (graph path disabled: no branch heads configured).
type recoverySource struct {
	mockSource
}

func newRecoveryFixtures(t *testing.T, commitCount int) (*recoverySource, *db.DB, *SyncConfig) {
	t.Helper()
	dbConn, err := db.OpenMemory()
	require.NoError(t, err)
	t.Cleanup(func() { dbConn.Close() })

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	commits := make([]model.Commit, commitCount)
	for i := range commits {
		commits[i] = model.Commit{
			Org: "o", Repo: "r", SHA: fmt.Sprintf("sha%03d", i),
			AuthorID: 7, AuthorLogin: "dev", AuthorEmail: "dev@x.com",
			CommittedAt: base.Add(time.Duration(i) * time.Minute),
			Message:     fmt.Sprintf("feat: change %d (#%d)", i, i),
			ParentCount: 1, Additions: 5, Deletions: 1,
		}
	}
	src := &recoverySource{mockSource{
		repos:   map[string][]model.RepoInfo{"o": {{Org: "o", Name: "r", DefaultBranch: "main"}}},
		commits: map[string][]model.Commit{"o/r/main": commits},
	}}
	cfg := &SyncConfig{Orgs: []OrgConfig{{Name: "o"}}, Concurrency: 1, EnrichConcurrency: 1}
	return src, dbConn, cfg
}

// approvedEnrichment yields a compliant enrichment: one merged PR with an
// independent approval on the head SHA.
func approvedEnrichment(org, repo, sha string) model.EnrichmentResult {
	merged := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	pr := model.PullRequest{
		Org: org, Repo: repo, Number: 1000, Merged: true,
		HeadSHA: "head-" + sha, AuthorID: 7, AuthorLogin: "dev", MergedAt: merged,
	}
	return model.EnrichmentResult{
		Commit: model.Commit{Org: org, Repo: repo, SHA: sha},
		PRs:    []model.PullRequest{pr},
		Reviews: []model.Review{{
			Org: org, Repo: repo, PRNumber: pr.Number, ReviewID: 1,
			ReviewerID: 42, ReviewerLogin: "rev", State: "APPROVED",
			CommitID: pr.HeadSHA, SubmittedAt: merged.Add(-time.Hour),
		}},
	}
}

// flakyEnricher fails the first failCalls EnrichCommits invocations, then
// behaves like a healthy enricher.
type flakyEnricher struct {
	failCalls int32
	calls     atomic.Int32
}

func (e *flakyEnricher) EnrichCommits(_ context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	if e.calls.Add(1) <= e.failCalls {
		return nil, errors.New("injected enrichment outage")
	}
	out := make([]model.EnrichmentResult, len(shas))
	for i, sha := range shas {
		out[i] = approvedEnrichment(org, repo, sha)
	}
	return out, nil
}

func auditCounts(t *testing.T, dbConn *db.DB) (total, compliant int) {
	t.Helper()
	require.NoError(t, dbConn.DB.QueryRow(
		`SELECT count(*), count(*) FILTER (WHERE is_compliant) FROM audit_results`).
		Scan(&total, &compliant))
	return total, compliant
}

// An enrichment outage aborts the run; commits are persisted but
// unaudited. A plain re-run must mop the backlog up and converge to the
// same verdicts an uninterrupted run would have produced — even though
// the fetch window finds nothing new.
func TestRecovery_EnrichmentOutageThenResumeConverges(t *testing.T) {
	src, dbConn, cfg := newRecoveryFixtures(t, 30)
	enricher := &flakyEnricher{failCalls: 1000} // outage for the whole first run

	p := NewPipeline(src, enricher, dbConn, cfg, slog.Default())
	err := p.Run(context.Background())
	require.Error(t, err, "the outage must surface, not be swallowed")

	total, _ := auditCounts(t, dbConn)
	assert.Zero(t, total, "no audit rows may exist for unenriched commits")
	var commitRows int
	require.NoError(t, dbConn.DB.QueryRow(`SELECT count(*) FROM commits`).Scan(&commitRows))
	assert.Equal(t, 30, commitRows, "fetched commits must survive the failed run")

	// Heal the enricher; plain re-run (same source data).
	enricher.calls.Store(enricher.failCalls) // next call succeeds
	require.NoError(t, p.Run(context.Background()))

	total, compliant := auditCounts(t, dbConn)
	assert.Equal(t, 30, total, "resume must audit the full backlog")
	assert.Equal(t, 30, compliant)
}

// Cancellation mid-run is the SIGINT story: whatever landed before the
// cancel must not poison the resume.
func TestRecovery_CancellationMidRunThenResumeConverges(t *testing.T) {
	src, dbConn, cfg := newRecoveryFixtures(t, 60)

	ctx, cancel := context.WithCancel(context.Background())
	cancelling := &cancellingEnricher{cancel: cancel, cancelAfter: 1}
	p := NewPipeline(src, cancelling, dbConn, cfg, slog.Default())
	_ = p.Run(ctx) // error or partial success both acceptable; DB state is what matters

	healthy := &flakyEnricher{}
	p2 := NewPipeline(src, healthy, dbConn, cfg, slog.Default())
	require.NoError(t, p2.Run(context.Background()))

	total, compliant := auditCounts(t, dbConn)
	assert.Equal(t, 60, total, "every commit must end up audited after resume")
	assert.Equal(t, 60, compliant)
}

// cancellingEnricher serves cancelAfter successful batches, then cancels
// the run's context and fails — simulating SIGINT landing mid-sweep.
type cancellingEnricher struct {
	cancel      context.CancelFunc
	cancelAfter int32
	calls       atomic.Int32
}

func (e *cancellingEnricher) EnrichCommits(_ context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	if e.calls.Add(1) > e.cancelAfter {
		e.cancel()
		return nil, context.Canceled
	}
	out := make([]model.EnrichmentResult, len(shas))
	for i, sha := range shas {
		out[i] = approvedEnrichment(org, repo, sha)
	}
	return out, nil
}

// A clean run followed by an identical re-run must be a no-op: same row
// count, same verdicts, no re-audit churn.
func TestRecovery_RepeatRunIsIdempotent(t *testing.T) {
	src, dbConn, cfg := newRecoveryFixtures(t, 20)
	enricher := &flakyEnricher{}

	p := NewPipeline(src, enricher, dbConn, cfg, slog.Default())
	require.NoError(t, p.Run(context.Background()))
	total1, compliant1 := auditCounts(t, dbConn)
	require.Equal(t, 20, total1)

	require.NoError(t, p.Run(context.Background()))
	total2, compliant2 := auditCounts(t, dbConn)
	assert.Equal(t, total1, total2)
	assert.Equal(t, compliant1, compliant2)

	calls := enricher.calls.Load()
	require.NoError(t, p.Run(context.Background()))
	assert.Equal(t, calls, enricher.calls.Load(),
		"a third run over a converged DB must not re-enrich anything")
}
