package github

import (
	"regexp"
	"sort"
	"strconv"
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
	// revertBranchRe matches the branch-name convention GitHub's "Revert"
	// button uses: `revert-<N>-<base-branch>`, where N is the number of the
	// PR being reverted (NOT the number of the revert PR itself).
	revertBranchRe = regexp.MustCompile(`^revert-(\d+)-`)
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

// ParseRevertBranchName returns the number of the pull request being reverted
// when `branch` follows GitHub's `revert-<N>-<base-branch>` convention, the
// one github.com's "Revert" button emits. Returns (0, false) for any other
// branch name, including branches that happen to start with `revert-` but
// don't carry a numeric PR reference.
//
// Note the number returned is the REVERTED PR's number, not the revert PR's.
// Callers use it to look up the reverted PR's merge_commit_sha, which is the
// SHA `ParseRevert`'s trailer would have produced if GH's button emitted one.
func ParseRevertBranchName(branch string) (int, bool) {
	m := revertBranchRe.FindStringSubmatch(branch)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// ResolveRevertedSHA returns the SHA of the commit being reverted by a
// ManualRevert, preferring the `This reverts commit <sha>` trailer from
// the commit message (what `git revert` emits) and falling back to looking
// up the reverted PR's merge_commit_sha via the `revert-<N>-<base-branch>`
// convention that GitHub's "Revert" button uses.
//
// associatedPRs is the set of PRs discovered via `GET /commits/{sha}/pulls`
// for the revert commit — typically a single PR, but the fallback iterates
// defensively in case a commit is linked to more than one.
//
// lookupPR resolves a reverted PR number to its full record (in particular,
// MergeCommitSHA). When nil or when the target PR has no merge SHA the
// fallback yields "". Returning an error from lookupPR surfaces to the
// caller so transient API failures can be distinguished from "no match".
//
// Returns ("", nil) when neither source yields a SHA — let the caller
// classify as `message-only`.
func ResolveRevertedSHA(message string, associatedPRs []model.PullRequest, lookupPR func(number int) (*model.PullRequest, error)) (string, error) {
	if m := manualRevertBodySHAre.FindStringSubmatch(message); m != nil {
		return m[1], nil
	}
	if lookupPR == nil {
		return "", nil
	}
	for _, pr := range associatedPRs {
		n, ok := ParseRevertBranchName(pr.HeadBranch)
		if !ok {
			continue
		}
		target, err := lookupPR(n)
		if err != nil {
			return "", err
		}
		if target != nil && target.MergeCommitSHA != "" {
			return target.MergeCommitSHA, nil
		}
	}
	return "", nil
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
