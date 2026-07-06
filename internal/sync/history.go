package sync

import (
	"context"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// recordIfRewritten classifies a branch-head move (retained prior head →
// current head, given GitHub's compare status) and, when it is a
// non-fast-forward rewrite, logs it and persists a HistoryRewrite. A
// fast-forward or unknown move is a no-op. Errors from the store are
// logged, never fatal: a missed record must not fail the sync.
func (p *Pipeline) recordIfRewritten(ctx context.Context, repo model.RepoInfo, branch, priorSHA, newSHA, compareStatus string) {
	if classifyHeadMove(priorSHA, newSHA, compareStatus) != HeadRewritten {
		return
	}
	p.logger.Warn("history rewrite detected (force-push): prior head is not an ancestor of the current head",
		"org", repo.Org, "repo", repo.Name, "branch", branch,
		"prior_sha", priorSHA, "new_sha", newSHA, "compare_status", compareStatus)
	rec := model.HistoryRewrite{
		Org: repo.Org, Repo: repo.Name, Branch: branch,
		PriorSHA: priorSHA, NewSHA: newSHA, CompareStatus: compareStatus, DetectedAt: time.Now(),
	}
	if err := p.store.RecordHistoryRewrite(ctx, rec); err != nil {
		p.logger.Warn("failed to record history rewrite", "error", err)
	}
}

// A HeadMove classifies how a protected branch's head moved between two
// syncs — the observable half of history-rewrite (force-push) detection.
//
// SLSA Source Track requires a protected branch to only ever advance to a
// DESCENDANT revision; `git push --force` is prohibited. A malicious
// insider can force-push to orphan (hide) commits that were once on the
// branch — laundering away unreviewed code or removing evidence — leaving
// a clean-looking tree that a single-snapshot audit cannot flag. The only
// way to detect it is to compare the current head against a RETAINED prior
// head via content-addressed ancestry (a forged parent changes the
// commit's own SHA, so reachability is non-forgeable). See tla/History.tla.
type HeadMove int

const (
	// HeadUnknown — no prior head was recorded, or the ancestry relation
	// could not be determined. Fails to "unknown", never to "safe".
	HeadUnknown HeadMove = iota
	// HeadUnchanged — the head did not move.
	HeadUnchanged
	// HeadFastForward — the current head descends from the prior head
	// (prior is reachable from current). The only sound "safe" move.
	HeadFastForward
	// HeadRewritten — the prior head is NOT reachable from the current
	// head: the branch was force-pushed (reset backwards or diverged),
	// orphaning whatever was uniquely on the prior history.
	HeadRewritten
)

// classifyHeadMove maps the retained prior head, the current head, and
// GitHub's compare status (base=prior ... head=current) onto a HeadMove.
//
// GitHub's compare `status` values (GET /repos/{o}/{r}/compare/{b}...{h}):
//
//	identical — base and head are the same commit
//	ahead     — head is ahead of base; base is an ancestor of head → fast-forward
//	behind    — head is behind base; the branch reset to an older commit → rewrite
//	diverged  — shared history but neither is an ancestor of the other → rewrite
//
// Anything else (empty, unexpected) is HeadUnknown — we never infer "safe"
// from an unrecognized status.
func classifyHeadMove(priorSHA, currentSHA, compareStatus string) HeadMove {
	if priorSHA == "" {
		return HeadUnknown
	}
	if priorSHA == currentSHA {
		return HeadUnchanged
	}
	switch compareStatus {
	case "identical", "ahead":
		return HeadFastForward
	case "behind", "diverged":
		return HeadRewritten
	default:
		return HeadUnknown
	}
}
