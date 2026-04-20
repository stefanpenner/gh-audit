package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyMerge(t *testing.T) {
	tests := []struct {
		name           string
		parentCount    int
		message        string
		committerLogin string
		isVerified     bool
		want           MergeKind
		wantVerif      string
	}{
		{
			name:        "root commit (0 parents)",
			parentCount: 0,
			want:        NotMerge,
			wantVerif:   "none",
		},
		{
			name:        "normal commit / squash (1 parent)",
			parentCount: 1,
			message:     "feat: add thing",
			want:        NotMerge,
			wantVerif:   "none",
		},
		{
			name:           "GitHub merge-pull-request button (web-flow + verified)",
			parentCount:    2,
			message:        "Merge pull request #123 from org/feature-branch\n\nAdd foo",
			committerLogin: "web-flow",
			isVerified:     true,
			want:           CleanMerge,
			wantVerif:      "verified-merge-bot",
		},
		{
			name:           "git default merge message — not trusted even with matching prefix (locally crafted)",
			parentCount:    2,
			message:        "Merge branch 'main' into feature",
			committerLogin: "dev",
			isVerified:     false,
			want:           DirtyMerge,
			wantVerif:      "dirty",
		},
		{
			name:           "spoofed merge-pull-request message (attacker-crafted, non-web-flow committer)",
			parentCount:    2,
			message:        "Merge pull request #999 from attacker/evil",
			committerLogin: "attacker",
			isVerified:     true,
			want:           DirtyMerge,
			wantVerif:      "dirty",
		},
		{
			name:           "web-flow committer but unverified",
			parentCount:    2,
			message:        "Merge pull request #42 from dev/branch",
			committerLogin: "web-flow",
			isVerified:     false,
			want:           DirtyMerge,
			wantVerif:      "dirty",
		},
		{
			name:        "2 parents, human-authored non-canned message",
			parentCount: 2,
			message:     "Resolving conflicts with master and fixing test",
			want:        DirtyMerge,
			wantVerif:   "dirty",
		},
		{
			name:        "2 parents, short cryptic message",
			parentCount: 2,
			message:     "wip",
			want:        DirtyMerge,
			wantVerif:   "dirty",
		},
		{
			name:        "octopus merge (3 parents)",
			parentCount: 3,
			message:     "Merge branches 'a', 'b' into main",
			want:        OctopusMerge,
			wantVerif:   "octopus",
		},
		{
			name:        "very large octopus",
			parentCount: 10,
			want:        OctopusMerge,
			wantVerif:   "octopus",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyMerge(tt.parentCount, tt.message, tt.committerLogin, tt.isVerified)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantVerif, mergeKindVerification(got))
		})
	}
}
