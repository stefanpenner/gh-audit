package github

import (
	"regexp"
	"strconv"
	"strings"
)

// MergeKind classifies a commit's relationship to merge operations based on
// its parent count and commit message.
//
// "Merge" here means an actual merge commit (two or more parents), not the
// generic sense of "merge a PR". Squash- and rebase-merged PRs produce
// single-parent commits and are classified as NotMerge.
type MergeKind int

const (
	// NotMerge — the commit has fewer than 2 parents. Squash / rebase /
	// direct push / root commit all land here.
	NotMerge MergeKind = iota
	// CleanMerge — exactly 2 parents, GitHub's `Merge pull request #…`
	// message, committer == web-flow, AND GitHub's signature verified.
	// These are produced by GitHub's merge button, which refuses on
	// conflicts, so the commit carries no committer-authored code.
	// The signature check prevents a local actor from spoofing web-flow
	// as committer — only GitHub holds the web-flow signing key.
	CleanMerge
	// DirtyMerge — 2-parent merge that fails the CleanMerge signal
	// (wrong committer, unverified, or non-matching message).
	// Conservatively assumed to be a human-crafted merge which may
	// contain conflict-resolution or edits. Needs review.
	DirtyMerge
	// OctopusMerge — 3+ parents. Rare; typically tooling-generated.
	// Not auto-classified as clean because verifying an N-way union
	// merge is more nuanced than the 2-parent case.
	OctopusMerge
)

// mergePullRequestPrefix is the canned prefix of GitHub's merge-button
// commit message. Other merge-looking messages (git's `Merge branch …`,
// `Merge remote-tracking branch …`) are not trusted because they can be
// produced locally with arbitrary content in the merge commit.
const mergePullRequestPrefix = "Merge pull request #"

// webFlowCommitter is the committer login GitHub uses for merge-button
// commits. Paired with a verified signature, it's a strong signal that
// the merge came from GitHub's server and not a local `git merge`.
const webFlowCommitter = "web-flow"

// ClassifyMerge returns the merge kind for the given commit.
//
// CleanMerge requires ALL of: 2 parents, `Merge pull request #…` message,
// committer == `web-flow`, AND GitHub's signature verified. Any missing
// signal downgrades a 2-parent commit to DirtyMerge. This prevents a local
// actor from forging a CleanMerge by crafting a matching message, since
// they cannot produce a web-flow-signed commit without compromising
// GitHub's signing key.
//
// 3+ parent (octopus) merges are classified separately; they're rare and
// usually tooling-generated but auto-classification is skipped for safety.
func ClassifyMerge(parentCount int, message, committerLogin string, isVerified bool) MergeKind {
	switch {
	case parentCount < 2:
		return NotMerge
	case parentCount >= 3:
		return OctopusMerge
	}
	// parentCount == 2
	if strings.HasPrefix(message, mergePullRequestPrefix) &&
		strings.EqualFold(committerLogin, webFlowCommitter) &&
		isVerified {
		return CleanMerge
	}
	return DirtyMerge
}

// prReferenceRE matches the trailing "(#N)" token that GitHub's
// squash-merge button appends to commit titles. Anchored to end-of-line
// so we pick the *last* token on the first line — guards against
// revert-of-squash titles like `Revert "Foo (#100)" (#101)` where the
// outer (#101) is the right answer.
var prReferenceRE = regexp.MustCompile(`\(#(\d+)\)\s*$`)

// ParsePRReference extracts the trailing PR number from a squash-merge
// commit message's first line. Returns (number, true) on a match;
// (0, false) for any message that does not end with `(#N)`.
//
// This is a *hint*, not a canonical lookup: a commit author can write
// any number into their message. It must always be paired with a
// canonical verification step (typically:
// `pulls/N.merge_commit_sha == sha`) before the link is trusted.
//
// Mirrors ParseRevert's role for revert messages.
func ParsePRReference(message string) (int, bool) {
	firstLine := message
	if i := strings.IndexByte(message, '\n'); i >= 0 {
		firstLine = message[:i]
	}
	m := prReferenceRE.FindStringSubmatch(firstLine)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// mergeKindVerification returns the string persisted to
// audit_results.merge_verification for a given kind. Parallel to
// revert_verification's vocabulary.
func mergeKindVerification(k MergeKind) string {
	switch k {
	case CleanMerge:
		return "verified-merge-bot"
	case DirtyMerge:
		return "dirty"
	case OctopusMerge:
		return "octopus"
	default:
		return "none"
	}
}
