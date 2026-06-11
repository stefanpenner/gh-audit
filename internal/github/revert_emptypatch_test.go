package github

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// GitHub omits the textual `patch` for binary files, very large diffs, and
// mode-only changes. An empty patch yields empty add/remove multisets, so
// two different binary blobs used to compare as a "perfect inverse" —
// letting a revert that swaps a binary for a DIFFERENT binary smuggle
// unreviewed bytes through as diff-verified. Unverifiable content must
// fail closed.
func TestIsCleanRevertDiff_EmptyPatchFailsClosed(t *testing.T) {
	textAdd := model.FileDiff{Filename: "a.txt", Status: "modified", Additions: 1, Patch: "@@ -0,0 +1 @@\n+hello"}
	textDel := model.FileDiff{Filename: "a.txt", Status: "modified", Deletions: 1, Patch: "@@ -1 +0,0 @@\n-hello"}

	t.Run("binary swap must not verify", func(t *testing.T) {
		// Both sides change logo.png; patches are empty (binary). The revert
		// could contain ANY blob — there is nothing to verify against.
		revert := []model.FileDiff{textDel, {Filename: "logo.png", Status: "modified"}}
		reverted := []model.FileDiff{textAdd, {Filename: "logo.png", Status: "modified"}}
		assert.False(t, IsCleanRevertDiff(revert, reverted),
			"empty-patch (binary/large/mode-only) files are unverifiable and must fail closed")
	})

	t.Run("text-only clean revert still verifies", func(t *testing.T) {
		revert := []model.FileDiff{textDel}
		reverted := []model.FileDiff{textAdd}
		assert.True(t, IsCleanRevertDiff(revert, reverted))
	})
}
