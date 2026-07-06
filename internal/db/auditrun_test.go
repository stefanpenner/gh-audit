package db

import (
	"context"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditRun_LatestWins(t *testing.T) {
	db := mustOpenMemory(t)
	ctx := context.Background()

	got, err := db.GetLatestAuditRun(ctx)
	require.NoError(t, err)
	assert.Nil(t, got, "no run stamped yet → (nil, nil), not an error")

	older := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	require.NoError(t, db.RecordAuditRun(ctx, model.AuditRun{
		FinishedAt: older, ToolVersion: "v1", ConfigFingerprint: "aaa", CommitsSynced: 10, CommitsAudited: 10,
	}))
	require.NoError(t, db.RecordAuditRun(ctx, model.AuditRun{
		FinishedAt: newer, ToolVersion: "v2", ConfigFingerprint: "bbb", CommitsSynced: 20, CommitsAudited: 18,
	}))

	got, err = db.GetLatestAuditRun(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "v2", got.ToolVersion, "latest run wins")
	assert.Equal(t, "bbb", got.ConfigFingerprint)
	assert.Equal(t, 18, got.CommitsAudited)
}
