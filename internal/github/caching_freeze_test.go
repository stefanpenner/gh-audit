package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// freezeFakeCache serves canned PR/review/commit rows, simulating DB state
// captured at an earlier sync.
type freezeFakeCache struct {
	stubEnrichmentCache
	pr        *model.PullRequest
	reviews   []model.Review
	prCommits []model.Commit
}

func (f freezeFakeCache) GetPullRequest(_ context.Context, _, _ string, _ int) (*model.PullRequest, error) {
	return f.pr, nil
}
func (f freezeFakeCache) GetReviewsForPR(_ context.Context, _, _ string, _ int) ([]model.Review, error) {
	return f.reviews, nil
}
func (f freezeFakeCache) GetCommitsForPR(_ context.Context, _, _ string, _ int) ([]model.Commit, error) {
	return f.prCommits, nil
}

// The merged-PR freeze must require the PR to actually BE merged. Rows
// snapshotted while a PR was open are a moment-in-time copy — trusting
// them skips the API and a later approval is never observed (false "no
// approval on final commit"). No current writer persists open-PR rows,
// but the freeze must not depend on that global invariant holding
// forever.
func TestGetReviews_OpenPRRowsAreNotFrozen(t *testing.T) {
	var apiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "state": "COMMENTED", "commit_id": "head1", "user": map[string]any{"login": "a", "id": 10}},
			{"id": 2, "state": "APPROVED", "commit_id": "head1", "user": map[string]any{"login": "b", "id": 20}},
		})
	}))
	defer srv.Close()

	cache := freezeFakeCache{
		pr: &model.PullRequest{Org: "testorg", Repo: "testrepo", Number: 7, Merged: false},
		reviews: []model.Review{
			{Org: "testorg", Repo: "testrepo", PRNumber: 7, ReviewID: 1, State: "COMMENTED", CommitID: "head1"},
		},
	}
	enricher := NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), cache)

	reviews, err := enricher.getReviews(context.Background(), "testorg", "testrepo", 7)
	require.NoError(t, err)
	assert.Equal(t, int32(1), apiCalls.Load(), "open-PR rows are a snapshot; the API must be consulted")
	require.Len(t, reviews, 2, "the post-snapshot approval must be visible")
}

func TestGetReviews_MergedPRRowsAreFrozen(t *testing.T) {
	var apiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
	}))
	defer srv.Close()

	cache := freezeFakeCache{
		pr: &model.PullRequest{Org: "testorg", Repo: "testrepo", Number: 7, Merged: true},
		reviews: []model.Review{
			{Org: "testorg", Repo: "testrepo", PRNumber: 7, ReviewID: 1, State: "APPROVED", CommitID: "head1"},
		},
	}
	enricher := NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), cache)

	reviews, err := enricher.getReviews(context.Background(), "testorg", "testrepo", 7)
	require.NoError(t, err)
	assert.Equal(t, int32(0), apiCalls.Load(), "merged-PR rows are frozen — no API call")
	require.Len(t, reviews, 1)
}

func TestGetPRCommits_OpenPRRowsAreNotFrozen(t *testing.T) {
	var apiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"sha": "bc1", "commit": map[string]any{"message": "one"}, "author": map[string]any{"login": "a", "id": 10}},
			{"sha": "bc2", "commit": map[string]any{"message": "two"}, "author": map[string]any{"login": "b", "id": 20}},
		})
	}))
	defer srv.Close()

	cache := freezeFakeCache{
		pr: &model.PullRequest{Org: "testorg", Repo: "testrepo", Number: 7, Merged: false},
		prCommits: []model.Commit{
			{Org: "testorg", Repo: "testrepo", SHA: "bc1", AuthorID: 10},
		},
	}
	enricher := NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), cache)

	commits, err := enricher.getPRCommits(context.Background(), "testorg", "testrepo", 7)
	require.NoError(t, err)
	assert.Equal(t, int32(1), apiCalls.Load(), "open-PR branch commits must be refetched")
	require.Len(t, commits, 2, "the post-snapshot push must be visible")
}
