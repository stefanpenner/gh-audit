package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEnrichmentCache is a no-op EnrichmentCache used to force the
// CachingEnricher down its API paths in tests. Every method returns
// empty/nil so the live HTTP server (httptest) drives every lookup.
type stubEnrichmentCache struct{}

func (stubEnrichmentCache) GetPullRequest(_ context.Context, _, _ string, _ int) (*model.PullRequest, error) {
	return nil, nil
}
func (stubEnrichmentCache) GetPRsForCommit(_ context.Context, _, _, _ string) ([]model.PullRequest, error) {
	return nil, nil
}
func (stubEnrichmentCache) GetReviewsForPR(_ context.Context, _, _ string, _ int) ([]model.Review, error) {
	return nil, nil
}
func (stubEnrichmentCache) GetCheckRunsForCommit(_ context.Context, _, _, _ string) ([]model.CheckRun, error) {
	return nil, nil
}
func (stubEnrichmentCache) GetCommitsForPR(_ context.Context, _, _ string, _ int) ([]model.Commit, error) {
	return nil, nil
}
func (stubEnrichmentCache) GetCommitsBySHA(_ context.Context, _, _ string, _ []string) ([]model.Commit, error) {
	return nil, nil
}

// TestRecoverPRFromMergeMessage_HappyPath confirms the parse + canonical
// verify fallback fires when /commits/{sha}/pulls returns empty and the
// squash-merge commit message ends with `(#N)` whose PR has a matching
// merge_commit_sha. Mirrors the production gap observed on
// linkedin-multiproduct/campaign-manager-api commit 07dbb6c0...
func TestRecoverPRFromMergeMessage_HappyPath(t *testing.T) {
	const sha = "07dbb6c012528e2248b936651d156ec628a56b27"
	commitFirstLine := "Add accountId param to findByStrategies (#12729)"

	var commitPullsCalls, commitDetailCalls, prDetailCalls atomic.Int32

	handler := http.NewServeMux()
	handler.HandleFunc("/repos/testorg/testrepo/commits/"+sha+"/pulls", func(w http.ResponseWriter, _ *http.Request) {
		commitPullsCalls.Add(1)
		_ = json.NewEncoder(w).Encode([]any{}) // GitHub's empty-index gap.
	})
	handler.HandleFunc("/repos/testorg/testrepo/commits/"+sha, func(w http.ResponseWriter, _ *http.Request) {
		commitDetailCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sha":      sha,
			"html_url": "https://github.com/testorg/testrepo/commit/" + sha,
			"commit": map[string]any{
				"message": commitFirstLine,
				"author":  map[string]any{"email": "dev@example.com", "date": "2026-03-30T16:24:17Z"},
			},
			"author":  map[string]any{"login": "shkotha", "id": 999001},
			"parents": []map[string]any{{"sha": "parent1"}},
		})
	})
	handler.HandleFunc("/repos/testorg/testrepo/pulls/12729", func(w http.ResponseWriter, _ *http.Request) {
		prDetailCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":           12729,
			"title":            "Add accountId param to findByStrategies",
			"merged":           true,
			"merge_commit_sha": sha, // ← the canonical proof
			"head":             map[string]any{"sha": "a60caea9", "ref": "feature"},
			"user":             map[string]any{"login": "shkotha"},
			"merged_by":        map[string]any{"login": "shkotha"},
			"merged_at":        "2026-03-30T16:24:18Z",
			"html_url":         "https://github.com/testorg/testrepo/pull/12729",
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	pool := mockTokenPool(t, srv.URL)
	client := NewClient(pool, testLogger())
	enricher := NewCachingEnricher(client, stubEnrichmentCache{})

	prs, err := enricher.getPRsForCommit(context.Background(), "testorg", "testrepo", sha)
	require.NoError(t, err)
	require.Len(t, prs, 1, "recovery should produce exactly one PR")
	assert.Equal(t, 12729, prs[0].Number)
	assert.True(t, prs[0].Merged)
	assert.Equal(t, sha, prs[0].MergeCommitSHA)

	assert.Equal(t, int32(1), commitPullsCalls.Load(), "/commits/{sha}/pulls called once (the cold path)")
	assert.Equal(t, int32(1), commitDetailCalls.Load(), "GetCommitDetail called once for the message")
	assert.Equal(t, int32(1), prDetailCalls.Load(), "GetPullRequest called once for canonical verification")
	assert.Equal(t, int64(1), enricher.Stats.PRRecovered.Load(), "PRRecovered incremented")
}

// TestRecoverPRFromMergeMessage_MismatchRejected guards the unforgeable
// step: even when a commit message claims `(#N)`, the link is rejected
// if PR #N's merge_commit_sha doesn't equal the audited SHA. A malicious
// or sloppy author cannot forge the association by writing any number
// into their message.
func TestRecoverPRFromMergeMessage_MismatchRejected(t *testing.T) {
	const sha = "deadbeefcafe1234567890abcdef0987654321ab"
	const fakePRNumber = 9999
	const realPRMergeSHA = "feedface0000000000000000000000000000face"
	commitFirstLine := "Spoofed (#" + atoiStr(fakePRNumber) + ")"

	handler := http.NewServeMux()
	handler.HandleFunc("/repos/testorg/testrepo/commits/"+sha+"/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{})
	})
	handler.HandleFunc("/repos/testorg/testrepo/commits/"+sha, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sha":     sha,
			"commit":  map[string]any{"message": commitFirstLine, "author": map[string]any{"date": "2026-03-30T16:24:17Z"}},
			"author":  map[string]any{"login": "attacker", "id": 999002},
			"parents": []map[string]any{{"sha": "parent1"}},
		})
	})
	handler.HandleFunc("/repos/testorg/testrepo/pulls/9999", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":           fakePRNumber,
			"merged":           true,
			"merge_commit_sha": realPRMergeSHA, // ← does NOT match the audited SHA
			"head":             map[string]any{"sha": "h", "ref": "r"},
			"user":             map[string]any{"login": "someone"},
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	enricher := NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), stubEnrichmentCache{})
	prs, err := enricher.getPRsForCommit(context.Background(), "testorg", "testrepo", sha)
	require.NoError(t, err)
	assert.Empty(t, prs, "verification must reject when PR.merge_commit_sha != commit SHA")
	assert.Equal(t, int64(0), enricher.Stats.PRRecovered.Load(), "no recovery when canonical verification fails")
}

// TestRecoverPRFromMergeMessage_NoPRReference confirms the recovery
// short-circuits cleanly on commits whose first line lacks `(#N)`: no
// extra commit-detail or PR-detail calls fire, behaviour matches the
// pre-recovery world (rule §3 will fire downstream).
func TestRecoverPRFromMergeMessage_NoPRReference(t *testing.T) {
	const sha = "0000000000000000000000000000000000000001"

	var commitDetailCalls, prDetailCalls atomic.Int32

	handler := http.NewServeMux()
	handler.HandleFunc("/repos/testorg/testrepo/commits/"+sha+"/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{})
	})
	handler.HandleFunc("/repos/testorg/testrepo/commits/"+sha, func(w http.ResponseWriter, _ *http.Request) {
		commitDetailCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sha":     sha,
			"commit":  map[string]any{"message": "feat: bump dep", "author": map[string]any{"date": "2026-03-30T16:24:17Z"}},
			"author":  map[string]any{"login": "dev", "id": 999003},
			"parents": []map[string]any{{"sha": "parent1"}},
		})
	})
	handler.HandleFunc("/repos/testorg/testrepo/pulls/", func(_ http.ResponseWriter, _ *http.Request) {
		prDetailCalls.Add(1)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	enricher := NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), stubEnrichmentCache{})
	prs, err := enricher.getPRsForCommit(context.Background(), "testorg", "testrepo", sha)
	require.NoError(t, err)
	assert.Empty(t, prs)
	assert.Equal(t, int32(1), commitDetailCalls.Load(), "message fetched once to inspect for (#N)")
	assert.Equal(t, int32(0), prDetailCalls.Load(), "no PR fetched when message lacks (#N)")
	assert.Equal(t, int64(0), enricher.Stats.PRRecovered.Load())
}

// atoiStr is a tiny helper that avoids dragging strconv into the test
// file just for one constant. Used to render fakePRNumber inline.
func atoiStr(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
