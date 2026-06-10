package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stefanpenner/gh-audit/internal/config"
)

func TestBuildSyncConfig_InvalidRepoFormat(t *testing.T) {
	cfg := config.Default()

	tests := []struct {
		name    string
		repos   []string
		wantErr string
	}{
		{
			name:    "missing slash",
			repos:   []string{"invalidrepo"},
			wantErr: `invalid --repo format "invalidrepo": expected org/repo`,
		},
		{
			name:    "empty org",
			repos:   []string{"/repo"},
			wantErr: `invalid --repo format "/repo": expected org/repo`,
		},
		{
			name:    "empty repo",
			repos:   []string{"org/"},
			wantErr: `invalid --repo format "org/": expected org/repo`,
		},
		{
			name:  "valid repo passes",
			repos: []string{"nodejs/node"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildSyncConfig(cfg, nil, tt.repos, "", "", 0)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
				require.Len(t, result.Orgs, 1)
			}
		})
	}
}

func TestBuildSyncConfig_Since(t *testing.T) {
	cfg := config.Default()
	repos := []string{"org/repo"}

	tests := []struct {
		name      string
		since     string
		wantSince time.Time
		wantErr   string
	}{
		{
			name:      "RFC3339 date",
			since:     "2023-01-02T03:04:05Z",
			wantSince: time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		{
			name:      "short date",
			since:     "2023-01-02",
			wantSince: time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC),
		},
		{
			name:      "epoch keyword",
			since:     "epoch",
			wantSince: epochSince,
		},
		{
			name:      "all keyword",
			since:     "all",
			wantSince: epochSince,
		},
		{
			name:      "beginning keyword",
			since:     "beginning",
			wantSince: epochSince,
		},
		{
			name:      "keyword is case-insensitive",
			since:     "Epoch",
			wantSince: epochSince,
		},
		{
			name:      "empty leaves zero (cursor/lookback decides)",
			since:     "",
			wantSince: time.Time{},
		},
		{
			name:    "invalid date errors",
			since:   "not-a-date",
			wantErr: "invalid --since",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildSyncConfig(cfg, nil, repos, tt.since, "", 0)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, result.Since.Equal(tt.wantSince),
				"since = %v, want %v", result.Since, tt.wantSince)
		})
	}
}

func TestEpochSinceIsReachableByGitHub(t *testing.T) {
	// determineSince only honours a non-zero Since; the epoch sentinel
	// must therefore be non-zero so "from the beginning" isn't mistaken
	// for "unset" and silently downgraded to the 90-day lookback.
	assert.False(t, epochSince.IsZero(), "epoch sentinel must be non-zero")
	assert.True(t, epochSince.Year() < 2000, "epoch sentinel must predate GitHub")
}
