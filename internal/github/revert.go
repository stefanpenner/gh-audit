package github

import (
	"regexp"
	"sort"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// RevertKind classifies a commit's relationship to revert operations based on
// its commit message alone.
type RevertKind int

const (
	// NotRevert — the message has no recognized revert prefix.
	NotRevert RevertKind = iota
	// AutoRevert — a bot-generated revert of the form "Automatic revert of <new>..<old>".
	// Trusted clean by construction: the generator emits pure inverses.
	AutoRevert
	// ManualRevert — a human-authored revert of the form `Revert "..."` with
	// a `This reverts commit <sha>` body line. Cleanliness must be verified
	// against the referenced commit's diff.
	ManualRevert
	// RevertOfRevert — a revert of a revert (re-application). Not a clean
	// revert regardless of verification — the code is coming back.
	RevertOfRevert
)

var (
	autoRevertRe            = regexp.MustCompile(`^Automatic revert of ([0-9a-f]{40})\.\.([0-9a-f]{40})`)
	manualRevertBodySHAre   = regexp.MustCompile(`This reverts commit ([0-9a-f]{40})`)
)

// ParseRevert inspects the commit message and returns the revert kind along
// with, where applicable, the SHA of the commit being reverted.
//
// For AutoRevert the returned SHA is the "old" SHA (second) from the
// "<new>..<old>" pair — that is, the commit whose state is being restored.
// For ManualRevert the SHA is extracted from the standard "This reverts
// commit <sha>" trailer emitted by `git revert`. An empty SHA with kind ==
// ManualRevert means the message looked like a revert but the body did not
// carry a parseable SHA trailer.
func ParseRevert(message string) (RevertKind, string) {
	// Revert-of-revert detection must handle both unescaped and
	// backslash-escaped inner quotes. `git revert` renders the reverted
	// commit's subject with embedded quotes escaped as `\"`, so a manual
	// revert of a manual revert looks like: Revert "Revert \"foo\""
	if strings.HasPrefix(message, `Revert "Revert "`) ||
		strings.HasPrefix(message, `Revert "Revert \"`) ||
		strings.HasPrefix(message, `Revert "Automatic revert of`) {
		return RevertOfRevert, ""
	}
	if m := autoRevertRe.FindStringSubmatch(message); m != nil {
		// m[1] = new SHA (the commit being reverted from master's view),
		// m[2] = old SHA (the commit whose state is being restored).
		// To verify an auto-revert we'd compare against m[1]; we don't
		// (policy: trust bot-generated auto-reverts).
		return AutoRevert, m[1]
	}
	if strings.HasPrefix(message, "Revert \"") {
		if m := manualRevertBodySHAre.FindStringSubmatch(message); m != nil {
			return ManualRevert, m[1]
		}
		return ManualRevert, ""
	}
	return NotRevert, ""
}

// IsCleanRevertDiff returns true iff the per-file patches in revertFiles are
// the exact inverse of the patches in revertedFiles: same set of filenames,
// and for every file the set of `+` lines in the revert equals the set of
// `-` lines in the reverted commit, and vice versa.
//
// Treats the additions and deletions as multisets — line order within a file
// doesn't matter, but duplicate lines must appear the same number of times on
// both sides. File headers (`+++`/`---`) and hunk markers (`@@`) are stripped
// before comparison.
func IsCleanRevertDiff(revertFiles, revertedFiles []model.FileDiff) bool {
	if len(revertFiles) != len(revertedFiles) {
		return false
	}
	rMap := make(map[string]string, len(revertFiles))
	for _, f := range revertFiles {
		rMap[f.Filename] = f.Patch
	}
	aMap := make(map[string]string, len(revertedFiles))
	for _, f := range revertedFiles {
		aMap[f.Filename] = f.Patch
	}
	if len(rMap) != len(aMap) {
		return false
	}
	for name := range rMap {
		aPatch, ok := aMap[name]
		if !ok {
			return false
		}
		rAdd, rDel := extractDiffLines(rMap[name])
		aAdd, aDel := extractDiffLines(aPatch)
		if !multisetEqual(rAdd, aDel) || !multisetEqual(rDel, aAdd) {
			return false
		}
	}
	return true
}

// extractDiffLines splits a unified-diff patch into the content of lines that
// were added vs. removed, ignoring file-header lines (`+++`, `---`) and
// hunk markers (`@@`). Returns the content without the leading `+` / `-`.
func extractDiffLines(patch string) (added, removed []string) {
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "@@"):
			continue
		case strings.HasPrefix(line, "+"):
			added = append(added, line[1:])
		case strings.HasPrefix(line, "-"):
			removed = append(removed, line[1:])
		}
	}
	return added, removed
}

// multisetEqual reports whether a and b contain the same elements with the
// same multiplicities, regardless of order.
func multisetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := make([]string, len(a))
	bb := make([]string, len(b))
	copy(aa, a)
	copy(bb, b)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
