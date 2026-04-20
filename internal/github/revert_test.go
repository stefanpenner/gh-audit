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
