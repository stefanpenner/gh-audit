package db

import (
	"context"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistoryRewrite_RoundTrips(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	r := model.HistoryRewrite{
		Org: "o", Repo: "r", Branch: "main",
		PriorSHA: "aaa", NewSHA: "bbb", CompareStatus: "diverged", DetectedAt: at,
	}
	require.NoError(t, db.RecordHistoryRewrite(ctx, r))
	// Idempotent: recording the same (prior,new) pair again does not duplicate.
	require.NoError(t, db.RecordHistoryRewrite(ctx, r))

	got, err := db.GetHistoryRewrites(ctx, "o", "r")
	require.NoError(t, err)
	require.Len(t, got, 1, "same rewrite recorded twice must not duplicate")
	assert.Equal(t, "main", got[0].Branch)
	assert.Equal(t, "aaa", got[0].PriorSHA)
	assert.Equal(t, "bbb", got[0].NewSHA)
	assert.Equal(t, "diverged", got[0].CompareStatus)
}
