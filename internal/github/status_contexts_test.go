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

func TestListStatusContexts_MapsStatesAndNegatesIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"state":       "failure",
			"total_count": 4,
			"statuses": []map[string]any{
				{"id": 11, "context": "jenkins/build", "state": "success", "updated_at": "2026-06-01T10:00:00Z"},
				{"id": 12, "context": "jenkins/deploy", "state": "failure", "updated_at": "2026-06-01T10:05:00Z"},
				{"id": 13, "context": "jenkins/lint", "state": "error", "updated_at": "2026-06-01T10:06:00Z"},
				{"id": 14, "context": "jenkins/slow", "state": "pending"},
			},
		})
	}))
	defer srv.Close()

	client := NewClient(mockTokenPool(t, srv.URL), testLogger())
	runs, err := client.ListStatusContexts(context.Background(), "testorg", "repo", "head1")
	require.NoError(t, err)
	require.Len(t, runs, 4)

	byName := map[string]model.CheckRun{}
	for _, r := range runs {
		byName[r.CheckName] = r
	}
	assert.Equal(t, "completed", byName["jenkins/build"].Status)
	assert.Equal(t, "success", byName["jenkins/build"].Conclusion)
	assert.Equal(t, int64(-11), byName["jenkins/build"].CheckRunID,
		"status ids are negated so they can never collide with check-run ids")
	assert.Equal(t, "failure", byName["jenkins/deploy"].Conclusion)
	assert.Equal(t, "error", byName["jenkins/lint"].Conclusion)
	assert.Equal(t, "in_progress", byName["jenkins/slow"].Status,
		"pending statuses must read as not-yet-concluded runs")
	assert.Empty(t, byName["jenkins/slow"].Conclusion)
}

// statusFreezeCache: no DB rows, PR unmerged — forces the API path.
type statusFreezeCache struct{ stubEnrichmentCache }

func TestGetCheckRuns_LegacyStatusSupplement(t *testing.T) {
	var checkRunCalls, statusCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/repo/commits/head1/check-runs", func(w http.ResponseWriter, r *http.Request) {
		checkRunCalls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"check_runs": []map[string]any{
				{"id": 5, "name": "unit-tests", "status": "completed", "conclusion": "success"},
			},
		})
	})
	mux.HandleFunc("/repos/testorg/repo/commits/head1/status", func(w http.ResponseWriter, r *http.Request) {
		statusCalls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"state": "success", "total_count": 1,
			"statuses": []map[string]any{
				{"id": 31, "context": "Owner Approval", "state": "success", "updated_at": "2026-06-01T10:00:00Z"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("missing required check pulls legacy statuses", func(t *testing.T) {
		enricher := NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), statusFreezeCache{})
		enricher.SetRequiredCheckNames([]string{"Owner Approval"})

		runs, err := enricher.getCheckRuns(context.Background(), "testorg", "repo", "head1", 7)
		require.NoError(t, err)
		require.Len(t, runs, 2, "check run + synthetic status context")
		assert.Equal(t, int32(1), statusCalls.Load())

		names := map[string]string{}
		for _, r := range runs {
			names[r.CheckName] = r.Conclusion
		}
		assert.Equal(t, "success", names["Owner Approval"],
			"a Jenkins-style status context must satisfy the required check")
	})

	t.Run("all required checks present skips the status call", func(t *testing.T) {
		statusCalls.Store(0)
		enricher := NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), statusFreezeCache{})
		enricher.SetRequiredCheckNames([]string{"unit-tests"})

		_, err := enricher.getCheckRuns(context.Background(), "testorg", "repo", "head1", 7)
		require.NoError(t, err)
		assert.Zero(t, statusCalls.Load(), "no extra API call when Checks API covers every required name")
	})

	t.Run("no required checks configured never calls statuses", func(t *testing.T) {
		statusCalls.Store(0)
		enricher := NewCachingEnricher(NewClient(mockTokenPool(t, srv.URL), testLogger()), statusFreezeCache{})

		_, err := enricher.getCheckRuns(context.Background(), "testorg", "repo", "head1", 7)
		require.NoError(t, err)
		assert.Zero(t, statusCalls.Load())
	})
}
