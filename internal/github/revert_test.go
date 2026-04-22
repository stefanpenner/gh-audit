package github

import (
	"testing"

	"github.com/stefanpenner/gh-audit/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestParseRevert(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantKind RevertKind
		wantSHA  string
	}{
		{
			name:     "not a revert",
			message:  "feat: add feature\n\nSome description",
			wantKind: NotRevert,
		},
		{
			name:     "auto-revert with two SHAs",
			message:  "Automatic revert of 921197f96e12b6e4f5c82104af0d83b7627ed99d..4eca5c7c3b6d1f9563e877a9484c87be6633b647\n\nAutomated safety revert.",
			wantKind: AutoRevert,
			wantSHA:  "921197f96e12b6e4f5c82104af0d83b7627ed99d",
		},
		{
			name:     "manual revert with body SHA",
			message:  "Revert \"feat: add foo (#123)\"\n\nThis reverts commit abcdef1234567890abcdef1234567890abcdef12.",
			wantKind: ManualRevert,
			wantSHA:  "abcdef1234567890abcdef1234567890abcdef12",
		},
		{
			name:     "manual revert without body SHA",
			message:  "Revert \"feat: add foo\"\n\nNo sha in the body.",
			wantKind: ManualRevert,
			wantSHA:  "",
		},
		{
			name:     "revert-of-manual-revert (re-apply) is not a revert",
			message:  "Revert \"Revert \\\"feat: add foo\\\"\"\n\nThis reverts commit 0000000000000000000000000000000000000000.",
			wantKind: RevertOfRevert,
		},
		{
			name:     "revert-of-auto-revert (re-apply) is not a revert",
			message:  "Revert \"Automatic revert of 921197f..4eca5c7\"\n\nThis reverts commit 0000000000000000000000000000000000000000.",
			wantKind: RevertOfRevert,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, sha := ParseRevert(tt.message)
			assert.Equal(t, tt.wantKind, kind)
			assert.Equal(t, tt.wantSHA, sha)
		})
	}
}

func TestParseRevertBranchName(t *testing.T) {
	// GH's "Revert" button names revert branches `revert-<N>-<base-branch>`
	// where N is the number of the PR being reverted. ParseRevertBranchName
	// extracts N so a no-trailer revert commit can still be diff-verified
	// by looking up PR N's merge_commit_sha.
	tests := []struct {
		name   string
		branch string
		want   int
		wantOK bool
	}{
		{"flat GH revert branch", "revert-29-scenario/4.5-gh-revert-clean-base", 29, true},
		{"revert of a revert (nested)", "revert-33-revert-29-scenario/4.5-clean", 33, true},
		{"plain feature branch", "feature/add-x", 0, false},
		{"starts with revert but no number", "revert-foo", 0, false},
		{"hyphen-bare prefix", "revert-", 0, false},
		{"empty branch", "", 0, false},
		{"leading whitespace defeats anchoring", "  revert-29-foo", 0, false},
		{"multi-digit number", "revert-18413-linkedin-multiproduct/frontend-api", 18413, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseRevertBranchName(tt.branch)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestResolveRevertedSHA(t *testing.T) {
	// ResolveRevertedSHA tries the `This reverts commit <sha>` trailer
	// first (what `git revert` emits) and falls back to looking up the
	// reverted PR's merge_commit_sha via the `revert-<N>-` branch-name
	// convention that GitHub's "Revert" button uses. Both sources produce
	// the same semantic SHA — the one a caller can feed into
	// IsCleanRevertDiff for verification.

	const trailerSHA = "abcdef1234567890abcdef1234567890abcdef12"
	const baseMergeSHA = "fedcba9876543210fedcba9876543210fedcba98"

	t.Run("trailer SHA wins when present", func(t *testing.T) {
		msg := `Revert "feat: foo"` + "\n\nThis reverts commit " + trailerSHA + "."
		prs := []model.PullRequest{{Number: 33, HeadBranch: "revert-29-foo"}}
		lookup := func(int) (*model.PullRequest, error) {
			t.Fatal("lookup should not be consulted when trailer is present")
			return nil, nil
		}
		got, err := ResolveRevertedSHA(msg, prs, lookup)
		assert.NoError(t, err)
		assert.Equal(t, trailerSHA, got)
	})

	t.Run("branch-name fallback when trailer absent", func(t *testing.T) {
		msg := `Revert "Scenario 4.5: base (for GH revert button, clean)" (#33)`
		prs := []model.PullRequest{{Number: 33, HeadBranch: "revert-29-scenario/4.5-clean"}}
		lookup := func(n int) (*model.PullRequest, error) {
			assert.Equal(t, 29, n, "lookup should target the reverted PR number from the branch")
			return &model.PullRequest{Number: 29, MergeCommitSHA: baseMergeSHA}, nil
		}
		got, err := ResolveRevertedSHA(msg, prs, lookup)
		assert.NoError(t, err)
		assert.Equal(t, baseMergeSHA, got)
	})

	t.Run("no trailer and no revert-shaped branch → empty", func(t *testing.T) {
		msg := `Revert "feat: foo"`
		prs := []model.PullRequest{{Number: 10, HeadBranch: "feature/foo"}}
		got, err := ResolveRevertedSHA(msg, prs, func(int) (*model.PullRequest, error) {
			t.Fatal("lookup should not be called when no branch matches")
			return nil, nil
		})
		assert.NoError(t, err)
		assert.Equal(t, "", got)
	})

	t.Run("branch matches but target PR has no merge SHA → empty", func(t *testing.T) {
		msg := `Revert "feat: foo"`
		prs := []model.PullRequest{{Number: 33, HeadBranch: "revert-29-foo"}}
		lookup := func(int) (*model.PullRequest, error) {
			return &model.PullRequest{Number: 29, MergeCommitSHA: ""}, nil
		}
		got, err := ResolveRevertedSHA(msg, prs, lookup)
		assert.NoError(t, err)
		assert.Equal(t, "", got)
	})

	t.Run("nil lookup skips the branch-name path", func(t *testing.T) {
		msg := `Revert "feat: foo"`
		prs := []model.PullRequest{{Number: 33, HeadBranch: "revert-29-foo"}}
		got, err := ResolveRevertedSHA(msg, prs, nil)
		assert.NoError(t, err)
		assert.Equal(t, "", got)
	})

	t.Run("lookup error surfaces to caller", func(t *testing.T) {
		msg := `Revert "feat: foo"`
		prs := []model.PullRequest{{Number: 33, HeadBranch: "revert-29-foo"}}
		lookup := func(int) (*model.PullRequest, error) {
			return nil, assert.AnError
		}
		_, err := ResolveRevertedSHA(msg, prs, lookup)
		assert.Error(t, err)
	})

	t.Run("first matching branch wins across multiple PRs", func(t *testing.T) {
		msg := `Revert "feat: foo"`
		prs := []model.PullRequest{
			{Number: 100, HeadBranch: "unrelated"},
			{Number: 33, HeadBranch: "revert-29-foo"},
			{Number: 34, HeadBranch: "revert-30-bar"}, // should not be consulted
		}
		calls := 0
		lookup := func(n int) (*model.PullRequest, error) {
			calls++
			return &model.PullRequest{Number: n, MergeCommitSHA: baseMergeSHA}, nil
		}
		got, err := ResolveRevertedSHA(msg, prs, lookup)
		assert.NoError(t, err)
		assert.Equal(t, baseMergeSHA, got)
		assert.Equal(t, 1, calls, "should stop at first successful resolution")
	})
}

func TestIsCleanRevertDiff(t *testing.T) {
	// Single-file clean revert: R adds what A removed and removes what A added.
	aPatch := "--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,4 @@\n line1\n-old line\n+new line\n line2\n"
	rPatch := "--- a/foo.go\n+++ b/foo.go\n@@ -1,4 +1,3 @@\n line1\n-new line\n+old line\n line2\n"

	revert := []model.FileDiff{{Filename: "foo.go", Patch: rPatch}}
	reverted := []model.FileDiff{{Filename: "foo.go", Patch: aPatch}}
	assert.True(t, IsCleanRevertDiff(revert, reverted), "expected single-file clean revert")

	// Multi-file clean revert
	bPatch := "--- a/bar.go\n+++ b/bar.go\n@@ -1 +0,0 @@\n-removed\n"
	brPatch := "--- a/bar.go\n+++ b/bar.go\n@@ -0,0 +1 @@\n+removed\n"
	revertM := []model.FileDiff{
		{Filename: "foo.go", Patch: rPatch},
		{Filename: "bar.go", Patch: brPatch},
	}
	revertedM := []model.FileDiff{
		{Filename: "foo.go", Patch: aPatch},
		{Filename: "bar.go", Patch: bPatch},
	}
	assert.True(t, IsCleanRevertDiff(revertM, revertedM), "expected multi-file clean revert")

	// Mismatch: different files
	revertedWrong := []model.FileDiff{{Filename: "baz.go", Patch: aPatch}}
	assert.False(t, IsCleanRevertDiff(revert, revertedWrong), "expected mismatch on differing filenames")

	// Mismatch: different line content
	rPatchWrong := "--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,4 @@\n line1\n-new line\n+DIFFERENT\n line2\n"
	revertW := []model.FileDiff{{Filename: "foo.go", Patch: rPatchWrong}}
	assert.False(t, IsCleanRevertDiff(revertW, reverted), "expected mismatch on differing lines")

	// Mismatch: different file count
	assert.False(t, IsCleanRevertDiff(revert, revertedM), "expected mismatch on differing file counts")

	// Duplicate lines must have same multiplicity
	dupR := []model.FileDiff{{Filename: "x.go", Patch: "@@\n+same\n+same\n"}}
	dupA := []model.FileDiff{{Filename: "x.go", Patch: "@@\n-same\n"}}
	assert.False(t, IsCleanRevertDiff(dupR, dupA), "expected mismatch on duplicate-line multiplicity")
}
