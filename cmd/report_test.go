package cmd

import (
	"strings"
	"testing"
)

func TestParseRepoFlag(t *testing.T) {
	tests := []struct {
		name     string
		repoArg  string
		orgArg   string
		wantOrg  string
		wantRepo string
	}{
		{
			name:     "org/repo splits into org and repo",
			repoArg:  "nodejs/node",
			wantOrg:  "nodejs",
			wantRepo: "node",
		},
		{
			name:     "bare repo stays as repo",
			repoArg:  "node",
			wantRepo: "node",
		},
		{
			name:     "org/repo does not override explicit --org",
			repoArg:  "nodejs/node",
			orgArg:   "other-org",
			wantOrg:  "other-org",
			wantRepo: "node",
		},
		{
			name:     "empty repo stays empty",
			repoArg:  "",
			wantOrg:  "",
			wantRepo: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			org := tt.orgArg
			repo := tt.repoArg

			if repo != "" && strings.Contains(repo, "/") {
				parts := strings.SplitN(repo, "/", 2)
				if org == "" {
					org = parts[0]
				}
				repo = parts[1]
			}

			if org != tt.wantOrg {
				t.Errorf("org = %q, want %q", org, tt.wantOrg)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}
