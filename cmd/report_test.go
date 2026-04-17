package cmd

import (
	"strings"
	"testing"

	"github.com/stefanpenner/gh-audit/internal/report"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRepoFlag(t *testing.T) {
	tests := []struct {
		name     string
		repos    []string
		orgArg   string
		wantOpts report.ReportOpts
	}{
		{
			name:  "single org/repo",
			repos: []string{"nodejs/node"},
			wantOpts: report.ReportOpts{
				Repos: []report.RepoFilter{{Org: "nodejs", Repo: "node"}},
			},
		},
		{
			name:  "multiple org/repos",
			repos: []string{"nodejs/node", "rails/rails"},
			wantOpts: report.ReportOpts{
				Repos: []report.RepoFilter{
					{Org: "nodejs", Repo: "node"},
					{Org: "rails", Repo: "rails"},
				},
			},
		},
		{
			name:     "org flag alone, no repos",
			repos:    nil,
			orgArg:   "nodejs",
			wantOpts: report.ReportOpts{Org: "nodejs"},
		},
		{
			name:  "org/repo does not override explicit --org",
			repos: []string{"nodejs/node"},
			orgArg:  "other-org",
			wantOpts: report.ReportOpts{
				Org:   "other-org",
				Repos: []report.RepoFilter{{Org: "nodejs", Repo: "node"}},
			},
		},
		{
			name:     "empty flags",
			repos:    nil,
			wantOpts: report.ReportOpts{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var repoFilters []report.RepoFilter
			for _, r := range tt.repos {
				if strings.Contains(r, "/") {
					parts := strings.SplitN(r, "/", 2)
					repoFilters = append(repoFilters, report.RepoFilter{Org: parts[0], Repo: parts[1]})
				}
			}

			opts := report.ReportOpts{
				Org:   tt.orgArg,
				Repos: repoFilters,
			}

			assert.Equal(t, tt.wantOpts.Org, opts.Org)
			require.Len(t, opts.Repos, len(tt.wantOpts.Repos))
			for i, rf := range opts.Repos {
				assert.Equal(t, tt.wantOpts.Repos[i], rf, "Repos[%d]", i)
			}
		})
	}
}
