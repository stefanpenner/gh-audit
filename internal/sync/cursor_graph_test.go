package sync

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stefanpenner/gh-audit/internal/github"
	"github.com/stefanpenner/gh-audit/internal/model"
)

func graphTestFixtures(lastSHA string, lastDate time.Time) (*mockSource, *mockStore, *mockEnricher, *SyncConfig) {
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"o": {{Org: "o", Name: "r", DefaultBranch: "main"}},
		},
		commits:        map[string][]model.Commit{},
		branchHeads:    map[string]string{},
		compareCommits: map[string][]model.Commit{},
	}
	store := newMockStore()
	store.cursors["o/r/main"] = &model.SyncCursor{
		Org: "o", Repo: "r", Branch: "main", LastDate: lastDate, LastSHA: lastSHA,
	}
	cfg := &SyncConfig{Orgs: []OrgConfig{{Name: "o"}}, Concurrency: 1}
	return source, store, &mockEnricher{}, cfg
}

// The graph path must ingest a commit whose committer date predates the
// cursor watermark — the exact commit the date-window `?since=` filter
// can never see (client-settable GIT_COMMITTER_DATE backdating).
func TestGraphCursor_IngestsBackdatedCommit(t *testing.T) {
	lastDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	source, store, enricher, cfg := graphTestFixtures("oldhead", lastDate)

	backdated := model.Commit{
		Org: "o", Repo: "r", SHA: "backdated", AuthorID: 1, AuthorLogin: "dev",
		CommittedAt: lastDate.Add(-30 * 24 * time.Hour), // a month before the watermark
		Message:     "smuggled", ParentCount: 1, Additions: 1,
	}
	source.branchHeads["o/r/main"] = "newhead"
	source.compareCommits["o/r/main oldhead...newhead"] = []model.Commit{backdated}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	require.NoError(t, p.Run(context.Background()))

	assert.Equal(t, int32(0), source.listCalls.Load(), "graph path must not fall back to date listing")
	assert.Equal(t, int32(1), source.compareCalls.Load())

	store.mu.Lock()
	defer store.mu.Unlock()
	shas := map[string]bool{}
	for _, c := range store.commits {
		shas[c.SHA] = true
	}
	assert.True(t, shas["backdated"], "backdated commit must be ingested via compare")

	cur := store.cursors["o/r/main"]
	require.NotNil(t, cur)
	assert.Equal(t, "newhead", cur.LastSHA, "cursor must advance to the new tip")
	assert.Equal(t, lastDate, cur.LastDate, "date watermark must not regress to the backdated commit")
}

// head == last_sha short-circuits to zero fetched commits — but the
// unaudited mop-up must still run.
func TestGraphCursor_UnchangedHeadSkipsFetchButMopsUp(t *testing.T) {
	lastDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	source, store, enricher, cfg := graphTestFixtures("samehead", lastDate)
	source.branchHeads["o/r/main"] = "samehead"

	// Backlog left by a previously-failed run: commit exists, no audit row.
	store.unaudited["o/r"] = []model.Commit{
		{Org: "o", Repo: "r", SHA: "leftover", AuthorID: 1, CommittedAt: lastDate, Additions: 1, ParentCount: 1},
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	require.NoError(t, p.Run(context.Background()))

	assert.Equal(t, int32(0), source.listCalls.Load())
	assert.Equal(t, int32(0), source.compareCalls.Load(), "unchanged head needs no compare call")

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.auditResults, 1, "mop-up must audit the leftover backlog")
	assert.Equal(t, "leftover", store.auditResults[0].SHA)
}

// A force-push (compare 404s) falls back to the date-window listing.
func TestGraphCursor_CompareUnavailableFallsBackToDateWindow(t *testing.T) {
	lastDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	source, store, enricher, cfg := graphTestFixtures("rewritten", lastDate)
	source.branchHeads["o/r/main"] = "newhead"
	source.compareErr = fmt.Errorf("%w: base gone", github.ErrCompareUnavailable)
	source.commits["o/r/main"] = []model.Commit{
		{Org: "o", Repo: "r", SHA: "tip", AuthorID: 1, CommittedAt: lastDate.Add(time.Hour), Additions: 1, ParentCount: 1},
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	require.NoError(t, p.Run(context.Background()))

	assert.Equal(t, int32(1), source.compareCalls.Load())
	assert.Equal(t, int32(1), source.listCalls.Load(), "must fall back to ListCommits")

	store.mu.Lock()
	defer store.mu.Unlock()
	cur := store.cursors["o/r/main"]
	require.NotNil(t, cur)
	assert.Equal(t, "tip", cur.LastSHA, "fallback records the first listed commit (the tip) as the new cursor SHA")
}

// An explicit --since override bypasses the graph path entirely.
func TestGraphCursor_ExplicitSinceUsesDateWindow(t *testing.T) {
	lastDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	source, store, enricher, cfg := graphTestFixtures("oldhead", lastDate)
	cfg.Since = lastDate.Add(-90 * 24 * time.Hour)
	source.commits["o/r/main"] = []model.Commit{}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	require.NoError(t, p.Run(context.Background()))

	assert.Equal(t, int32(0), source.compareCalls.Load())
	assert.Equal(t, int32(1), source.listCalls.Load())
}

// A zero-commit date-window run must not blank the stored tip SHA — that
// would silently downgrade the next run from graph compare
// (backdating-immune) to the date window.
func TestGraphCursor_ZeroCommitWindowKeepsLastSHA(t *testing.T) {
	lastDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	source, store, enricher, cfg := graphTestFixtures("storedtip", lastDate)
	// GetBranchHead errors (no entry) → date-window fallback; the window
	// returns zero commits.
	source.commits["o/r/main"] = []model.Commit{}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	require.NoError(t, p.Run(context.Background()))

	store.mu.Lock()
	defer store.mu.Unlock()
	cur := store.cursors["o/r/main"]
	require.NotNil(t, cur)
	assert.Equal(t, "storedtip", cur.LastSHA,
		"a quiet fallback window must not erase the graph cursor")
}
