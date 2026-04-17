package cmd

import (
	"testing"

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
