package sync

import (
	"fmt"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/github"
	"github.com/stefanpenner/gh-audit/internal/model"
)

// Audit evaluation kernel — Architecture.md §1–§8.
//
// Rules dispatch as follows:
//
//	§1 Exempt author            → applyExemptAuthorRule
//	§2 Empty commit             → applyEmptyCommitFallback (lazy; runs on both no-PR and post-loop paths)
//	§3 Has associated PR        → inlined in EvaluateCommit
//	§4 Approval on final commit → evaluatePR + latestReviewStatesOnFinal
//	§5 Self-approval exclusion  → evaluatePR + isSelfApproval
//	§6 Required status checks   → evaluatePR + evaluateRequiredChecks
//	§7 Verdict across PRs       → EvaluateCommit loop + betterVerdict + finalize*
//	§8 Clean-revert waiver      → evaluateRevertCompliance
//
// File layout (functions grouped by role, not rule number):
//
//	Orchestration             — EvaluateCommit, initAuditResult
//	Rule-dispatch helpers     — applyExemptAuthorRule, applyEmptyCommitFallback,
//	                            evaluateRevertCompliance
//	Per-PR verdict machinery  — prVerdict, evaluatePR, betterVerdict, and the
//	                            three finalizers (Compliant / RevertWaiver / NonCompliant)
//	Leaf predicates           — latestReviewStatesOnFinal, evaluateRequiredChecks,
//	                            isSelfApproval
//	Pure utilities            — truncateSHA, hasNonExemptPRContributors, distinct*,
//	                            classifyMergeStrategy

// RequiredCheck describes a status check that must pass for compliance.
type RequiredCheck struct {
	Name       string
	Conclusion string
}

// StatsTrigger labels which audit rule triggered a lazy stats lookup.
// Threaded through StatsFetcher so the implementation can split the
// lazy commit_detail counter by trigger — useful for deciding whether
// eager batched prefetching of additions/deletions would pay off, or
// whether a different path (§5 PR-branch-author empty-stats lookup)
// dominates and needs separate handling.
type StatsTrigger string

const (
	// StatsTriggerEmptyCommit is rule §2's empty-commit fallback —
	// fired when an otherwise non-compliant commit has zero local
	// additions/deletions and we need GetCommitDetail to confirm
	// before letting the empty-commit waiver fire.
	StatsTriggerEmptyCommit StatsTrigger = "empty"
	// StatsTriggerSelfApproval is rule §5's "did this PR-branch
	// author actually contribute code?" disambiguation — fired
	// when a reviewer's PR-branch commits all look zero-stat
	// locally and we need GetCommitDetail to tell whether to drop
	// them from the self-approval check.
	StatsTriggerSelfApproval StatsTrigger = "self"
)

// StatsFetcher resolves a commit's additions/deletions. Used by
// EvaluateCommit for the §2 empty-commit fallback and the §5 PR-branch-
// author empty-stats disambiguation. Implementations should check the
// DB first and fall through to the REST API; returning any error
// leaves the stats at whatever the caller passed in (typically zero).
type StatsFetcher func(trigger StatsTrigger, org, repo, sha string) (additions, deletions int, err error)

// ----- Orchestration -----

// EvaluateCommit determines compliance for a single commit by running the
// audit rule list documented in Architecture.md §1–§8. The function body
// reads as that rule list top-to-bottom; each rule delegates to a small
// helper whose name points back to the Architecture section.
//
// Rule numbers are evaluated in decision-tree order, not numeric order:
//   - §2 (empty commit) is lazy and fires on both no-PR and post-loop paths.
//   - §7 (verdict) owns the PR loop AND the non-compliant fallback return.
//
// Performance notes:
//   - Eager prefetching of additions/deletions previously accounted for ~16%
//     of REST traffic on full sweeps; fetchStats lets the §2 fallback resolve
//     stats lazily on already-suspect commits only. A nil fetchStats is
//     tolerated — the fallback then uses the caller-supplied stats as-is.
//   - §8 (clean-revert waiver) is standalone: it judges the revert on its own
//     signals and does not inspect the reverted commit's verdict. See
//     TODO.md for the stricter cross-commit variant.
func EvaluateCommit(commit model.Commit, enrichment model.EnrichmentResult, exemptAuthors []model.ExemptAuthor, requiredChecks []RequiredCheck, fetchStats StatsFetcher) model.AuditResult {
	// Informational fields shared by every return path (revert/merge
	// signals, annotations, IsBot). Populated once, before any rule runs.
	result := initAuditResult(commit, enrichment)

	// §1 — Exempt author.
	if applyExemptAuthorRule(&result, commit, enrichment, exemptAuthors) {
		return result
	}

	// §3 — Has associated PR.
	if len(enrichment.PRs) == 0 {
		result.HasPR = false
		// §2 — Empty commit (no-PR path).
		if applyEmptyCommitFallback(&result, &commit, fetchStats) {
			return result
		}
		result.IsCompliant = false
		result.Reasons = []string{"no associated pull request"}
		result.MergeStrategy = classifyMergeStrategy(commit, false)
		return result
	}
	result.HasPR = true
	result.PRCount = len(enrichment.PRs)

	// §7 — verdict across associated PRs. evaluatePR folds §§4+5 into
	// approvalOnFinal (non-self APPROVED on pr.HeadSHA) and §6 into
	// ownerApprovalOK. The first PR that clears both wins: the commit is
	// compliant. Otherwise track the closest-to-compliant verdict so
	// finalizeNonCompliant can report its reasons.
	var best prVerdict
	for i := range enrichment.PRs {
		v := evaluatePR(commit, enrichment, &enrichment.PRs[i], requiredChecks, fetchStats, exemptAuthors)
		if v.approvalOnFinal && v.ownerApprovalOK {
			return finalizeCompliantPR(result, commit, enrichment, v)
		}
		if betterVerdict(v, best) {
			best = v
		}
	}

	// §2 — Empty commit (post-loop path). A PR that never modified bytes
	// is trivially compliant regardless of review state.
	if applyEmptyCommitFallback(&result, &commit, fetchStats) {
		return result
	}

	// §8 — Clean-revert waiver (standalone).
	if ok, reason := evaluateRevertCompliance(commit, enrichment); ok {
		return finalizeRevertWaiver(result, commit, enrichment, best, reason)
	}

	// §7 — Non-compliant verdict, reporting the best PR's reasons.
	return finalizeNonCompliant(result, commit, enrichment, best)
}

// initAuditResult populates the fields shared by every return path: Org/
// Repo/SHA, the informational revert and merge signals, reviewer-facing
// annotations, and the IsBot flag. None of these affect IsCompliant.
func initAuditResult(commit model.Commit, enrichment model.EnrichmentResult) model.AuditResult {
	result := model.AuditResult{
		Org:                commit.Org,
		Repo:               commit.Repo,
		SHA:                commit.SHA,
		CommitHref:         commit.Href,
		IsCleanRevert:      enrichment.IsCleanRevert,
		RevertVerification: enrichment.RevertVerification,
		RevertedSHA:        enrichment.RevertedSHA,
		IsCleanMerge:       enrichment.IsCleanMerge,
		MergeVerification:  enrichment.MergeVerification,
		Annotations:        ComputeAnnotations(commit, enrichment),
	}
	if strings.HasSuffix(strings.ToLower(commit.AuthorLogin), "[bot]") {
		result.IsBot = true
	}
	return result
}

// ----- Rule-dispatch helpers -----

// applyExemptAuthorRule implements Architecture.md §1. Returns true iff the
// commit is waived as exempt (caller should return result as-is). When the
// commit author matches but the PR contains non-exempt contributors (squash
// merge with human work), the exemption is NOT granted — IsExemptAuthor is
// still marked so reviewers can see the match — and the function returns
// false so downstream rules audit the human code.
//
// Matching prefers the unforgeable numeric account id; for service accounts
// whose emails GitHub doesn't bind to an account (commit.AuthorID == 0) the
// rule falls back to the operator-curated `verified_emails` list. See
// isExemptCommit for the full rationale.
func applyExemptAuthorRule(result *model.AuditResult, commit model.Commit, enrichment model.EnrichmentResult, exemptAuthors []model.ExemptAuthor) bool {
	if !isExemptCommit(commit.AuthorID, commit.AuthorEmail, exemptAuthors) {
		return false
	}
	result.IsExemptAuthor = true
	if hasNonExemptPRContributors(enrichment, exemptAuthors) {
		return false
	}
	result.IsCompliant = true
	result.Reasons = []string{"exempt: configured author"}
	result.MergeStrategy = classifyMergeStrategy(commit, false)
	return true
}

// isExemptCommit decides whether a commit's authorship matches the
// exempt list. The preferred signal is the numeric account id —
// GitHub-controlled, immutable, never reused, and not forgeable
// client-side. When the id is unavailable (AuthorID == 0, typical for
// service accounts whose git-author email isn't bound to a GitHub
// account) the function falls back to matching the commit's
// author email against any exempt entry's curated `verified_emails`
// list. The email path is forgeable in isolation (any local committer
// can set their git-author email arbitrarily), so callers that want
// to extend a carve-out beyond the audited commit itself — e.g. to a
// squash-merged PR's branch contents — must additionally verify every
// PR-branch commit passes this same check (see
// hasNonExemptPRContributors). The combination "operator-vetted email
// list + every contributor passes" recovers the same trust property
// id-only matching provided.
func isExemptCommit(authorID int64, authorEmail string, exemptAuthors []model.ExemptAuthor) bool {
	if authorID != 0 {
		for _, e := range exemptAuthors {
			if e.ID != 0 && e.ID == authorID {
				return true
			}
		}
		return false
	}
	if authorEmail == "" {
		return false
	}
	for _, e := range exemptAuthors {
		for _, em := range e.VerifiedEmails {
			if strings.EqualFold(em, authorEmail) {
				return true
			}
		}
	}
	return false
}

// applyEmptyCommitFallback implements Architecture.md §2. It flips `result`
// to the "empty commit" compliant state when the commit's net diff is zero
// lines, and returns true iff the fallback fired (caller should return
// `result` unchanged).
//
// Stats are resolved lazily: if the commit already carries non-zero
// additions/deletions the check short-circuits without IO; otherwise
// fetchStats is called (DB-first, API-fallback). A nil fetchStats or a
// fetch error leaves the caller's existing zero stats in place, which means
// the commit IS marked empty — matching the legacy "stats default to zero →
// treated as empty" semantics so this refactor is a strict subset.
func applyEmptyCommitFallback(result *model.AuditResult, commit *model.Commit, fetchStats StatsFetcher) bool {
	if commit.Additions != 0 || commit.Deletions != 0 {
		return false
	}
	if fetchStats != nil {
		if adds, dels, err := fetchStats(StatsTriggerEmptyCommit, commit.Org, commit.Repo, commit.SHA); err == nil {
			commit.Additions, commit.Deletions = adds, dels
		}
	}
	if commit.Additions != 0 || commit.Deletions != 0 {
		return false
	}
	result.IsEmptyCommit = true
	result.IsCompliant = true
	result.Reasons = []string{"empty commit"}
	result.MergeStrategy = classifyMergeStrategy(*commit, false)
	return true
}

// evaluateRevertCompliance implements Architecture.md §8. It returns
// (true, reason) iff the commit is a clean revert — either an AutoRevert
// (trusted by construction) or a ManualRevert whose diff was verified as
// the exact inverse of the reverted commit. Returns (false, "") otherwise.
//
// Conflict-resolved GH-UI reverts intentionally do NOT waive here — the
// diff is no longer a pure inverse, so reviewers should eyeball the
// conflict resolution. Revert-of-revert is likewise not treated as clean
// (content is coming *back* onto master, not off it). See TODO.md for
// deferred variants (re-apply diff verification, cross-commit chain).
func evaluateRevertCompliance(commit model.Commit, enrichment model.EnrichmentResult) (bool, string) {
	if !enrichment.IsCleanRevert {
		return false, ""
	}
	if enrichment.RevertedSHA != "" {
		return true, fmt.Sprintf("clean revert of %s", truncateSHA(enrichment.RevertedSHA))
	}
	return true, "clean revert"
}

// ----- Per-PR verdict machinery -----

// A prVerdict captures how one PR scores against Architecture.md §§4–6.
//
// The APPROVED reviews on pr.HeadSHA partition into two independent flags:
//   - approvalOnFinal — at least one non-self APPROVED on head SHA (§4 pass)
//   - selfApproved   — at least one self APPROVED on head SHA (§5 taint)
//
// Both, one, or neither may be true; they are disjoint signals and each
// emits its own reason string when §4 fails.
//
// staleApproval separately records a non-self APPROVED on an older SHA —
// a §4 presentational hint meaning "reviewed, then code changed." It is
// only computed when approvalOnFinal is false.
//
// ownerApproval / ownerApprovalOK carry the §6 result ("", "success",
// "failure", "missing") and its passing predicate.
//
// The struct is reused as (a) the early-return payload for a compliant PR
// and (b) the closest-to-compliant candidate tracked across PRs when no
// PR is compliant (see betterVerdict).
type prVerdict struct {
	pr               *model.PullRequest
	reasons          []string
	approvers        []string
	prBranchCommits  []model.Commit // PR-branch commits (for self-approval ID check)
	approvalOnFinal  bool     // §4: non-self APPROVED on pr.HeadSHA
	selfApproved     bool     // §5: any APPROVED whose reviewer is a code author
	staleApproval    bool     // §4: non-self APPROVED on older SHA
	postMergeConcern bool     // §4 cutoff: post-merge CHANGES_REQUESTED/DISMISSED
	ownerApproval    string   // §6: "", "success", "failure", "missing"
	ownerApprovalOK  bool     // §6: ownerApproval is "" or "success"
}

// evaluatePR scores a single PR against Architecture.md §§4–6 and returns
// a prVerdict. Side-effect free.
//
// The function runs as four phases:
//
//	Phase 1: classify APPROVED reviews on pr.HeadSHA as self vs. independent.
//	Phase 2: if no independent approval on head, look for one on an older
//	         SHA (stale-approval signal).
//	Phase 3: emit §4 / §5 reasons. selfApproved and staleApproval are
//	         independent flaws and can both appear for the same PR.
//	Phase 4: evaluate §6 required status checks and append any failure.
func evaluatePR(commit model.Commit, enrichment model.EnrichmentResult, pr *model.PullRequest, requiredChecks []RequiredCheck, fetchStats StatsFetcher, exemptAuthors []model.ExemptAuthor) prVerdict {
	v := prVerdict{pr: pr}
	v.prBranchCommits = filterNonEmptyContributors(commit.Org, commit.Repo, enrichment.PRBranchCommits[pr.Number], fetchStats)

	// Phase 1 — per-reviewer latest state on pr.HeadSHA with merge-time cutoff.
	latestByReviewer, postMergeConcern := latestReviewStatesOnFinal(enrichment.Reviews, *pr)
	v.postMergeConcern = postMergeConcern
	for _, review := range latestByReviewer {
		if review.State != "APPROVED" {
			continue
		}
		if review.ReviewerID == 0 {
			continue
		}
		if isSelfApproval(review, commit, *pr, v.prBranchCommits) {
			v.selfApproved = true
			continue
		}
		v.approvalOnFinal = true
		v.approvers = append(v.approvers, review.ReviewerLogin)
	}

	// Phase 2 — stale-approval detection (only meaningful when no fresh approval).
	if !v.approvalOnFinal {
		for _, review := range enrichment.Reviews {
			if review.PRNumber != pr.Number || review.CommitID == pr.HeadSHA {
				continue
			}
			if review.ReviewerID == 0 {
				continue
			}
			if review.State != "APPROVED" || isSelfApproval(review, commit, *pr, v.prBranchCommits) {
				continue
			}
			// Carve-out: if every PR-branch commit committed strictly
			// after this approval is authored by an exempt-list
			// account (typically a CI bot performing automated
			// branch-sync merges), the head moved without adding any
			// human-authored bytes the reviewer didn't see — the
			// approval's intent still covers the merged content.
			// Promote the review to approvalOnFinal so §4 doesn't fire.
			if isApprovalRefreshable(review, enrichment.PRBranchCommits[pr.Number], pr.MergeCommitSHA, exemptAuthors) {
				v.approvalOnFinal = true
				v.approvers = append(v.approvers, review.ReviewerLogin)
				continue
			}
			v.staleApproval = true
			break
		}
	}

	// Phase 3 — reasons for §4 / §5 failures. selfApproved and staleApproval
	// are independent; both may emit. "No approval on final commit" is only
	// the reason when neither flag fired — a genuine never-reviewed PR.
	if !v.approvalOnFinal {
		if v.selfApproved {
			v.reasons = append(v.reasons, fmt.Sprintf("self-approved (reviewer is code author) (PR #%d)", pr.Number))
		}
		if v.staleApproval {
			v.reasons = append(v.reasons, fmt.Sprintf("approval is stale — not on final commit (PR #%d)", pr.Number))
		}
		if !v.selfApproved && !v.staleApproval {
			v.reasons = append(v.reasons, fmt.Sprintf("no approval on final commit (PR #%d)", pr.Number))
		}
	}

	// Phase 4 — §6 required status checks.
	v.ownerApproval = evaluateRequiredChecks(enrichment.CheckRuns, pr.HeadSHA, requiredChecks)
	v.ownerApprovalOK = v.ownerApproval == "" || v.ownerApproval == "success"
	if !v.ownerApprovalOK {
		v.reasons = append(v.reasons, fmt.Sprintf("Owner Approval check missing/failed (PR #%d)", pr.Number))
	}

	return v
}

// betterVerdict reports whether candidate should replace best in the
// "closest to compliant" tournament (Architecture.md §7). Fewer reasons
// wins; ties break by higher PR number for deterministic reporting.
func betterVerdict(candidate, best prVerdict) bool {
	if best.pr == nil {
		return true
	}
	if len(candidate.reasons) != len(best.reasons) {
		return len(candidate.reasons) < len(best.reasons)
	}
	return candidate.pr.Number > best.pr.Number
}

// finalizeCompliantPR sets the compliant verdict from a PR that passed
// both §4 (non-self approval on final commit) and §6 (required checks).
func finalizeCompliantPR(result model.AuditResult, commit model.Commit, enrichment model.EnrichmentResult, v prVerdict) model.AuditResult {
	result.IsCompliant = true
	result.HasFinalApproval = true
	result.HasPostMergeConcern = v.postMergeConcern
	result.ApproverLogins = v.approvers
	result.OwnerApprovalCheck = v.ownerApproval
	result.PRNumber = v.pr.Number
	result.PRHref = v.pr.Href
	result.Reasons = []string{"compliant"}
	result.MergeStrategy = classifyMergeStrategy(commit, true)
	result.PRCommitAuthorLogins = distinctPRBranchAuthors(enrichment.PRBranchCommits)
	return result
}

// finalizeRevertWaiver sets the compliant verdict from Architecture.md §8.
// The best PR (if any) is surfaced for reporting without claiming final
// approval — the waiver rests on revert provenance, not on review state.
func finalizeRevertWaiver(result model.AuditResult, commit model.Commit, enrichment model.EnrichmentResult, best prVerdict, reason string) model.AuditResult {
	result.IsCompliant = true
	result.HasFinalApproval = false
	if best.pr != nil {
		result.PRNumber = best.pr.Number
		result.PRHref = best.pr.Href
	}
	result.Reasons = []string{reason}
	result.MergeStrategy = classifyMergeStrategy(commit, best.pr != nil)
	result.PRCommitAuthorLogins = distinctPRBranchAuthors(enrichment.PRBranchCommits)
	return result
}

// finalizeNonCompliant sets the negative verdict using the best PR's
// signals (Architecture.md §7). HasFinalApproval is recomputed from the
// fallback review-state map so reports can still list an independent
// approval even when the commit fails for other reasons (e.g. owner check).
//
// IsSelfApproved is set only when a self-approval exists AND no independent
// approval covered it (`!best.approvalOnFinal`). If both kinds of APPROVED
// appear on the head SHA the commit would have early-returned compliant
// via finalizeCompliantPR — unless §6 failed, in which case §4 is satisfied
// and the self-approval is purely informational, not the cause of failure.
func finalizeNonCompliant(result model.AuditResult, commit model.Commit, enrichment model.EnrichmentResult, best prVerdict) model.AuditResult {
	result.IsCompliant = false
	result.IsSelfApproved = best.selfApproved && !best.approvalOnFinal
	result.HasStaleApproval = best.staleApproval
	result.HasPostMergeConcern = best.postMergeConcern
	if best.pr != nil {
		result.PRNumber = best.pr.Number
		result.PRHref = best.pr.Href
		result.ApproverLogins = best.approvers
		result.OwnerApprovalCheck = best.ownerApproval
		fallbackLatest, _ := latestReviewStatesOnFinal(enrichment.Reviews, *best.pr)
		for _, review := range fallbackLatest {
			if review.State == "APPROVED" && review.ReviewerID != 0 && !isSelfApproval(review, commit, *best.pr, best.prBranchCommits) {
				result.HasFinalApproval = true
				break
			}
		}
	}
	result.Reasons = best.reasons
	result.MergeStrategy = classifyMergeStrategy(commit, true)
	result.PRCommitAuthorLogins = distinctPRBranchAuthors(enrichment.PRBranchCommits)
	return result
}

// ----- Leaf predicates -----

// latestReviewStatesOnFinal folds a PR's reviews into per-reviewer latest
// state on pr.HeadSHA, honouring the merge-time cutoff. Reviews submitted
// after pr.MergedAt are excluded from the map; a post-merge DISMISSED or
// CHANGES_REQUESTED instead sets the second return value for caller to
// surface as HasPostMergeConcern. A later COMMENTED review never clobbers
// an earlier APPROVED from the same reviewer (matches GitHub's UI, where
// commenting after approving leaves the approval intact).
func latestReviewStatesOnFinal(reviews []model.Review, pr model.PullRequest) (map[string]model.Review, bool) {
	latest := make(map[string]model.Review)
	postMergeConcern := false
	for _, review := range reviews {
		if review.PRNumber != pr.Number || review.CommitID != pr.HeadSHA || review.ReviewerID == 0 {
			continue
		}
		if !pr.MergedAt.IsZero() && review.SubmittedAt.After(pr.MergedAt) {
			if review.State == "CHANGES_REQUESTED" || review.State == "DISMISSED" {
				postMergeConcern = true
			}
			continue
		}
		key := reviewerKey(review)
		existing, exists := latest[key]
		if !exists {
			latest[key] = review
			continue
		}
		if review.SubmittedAt.Before(existing.SubmittedAt) {
			continue
		}
		if review.SubmittedAt.Equal(existing.SubmittedAt) && review.ReviewID <= existing.ReviewID {
			continue
		}
		if review.State == "COMMENTED" && existing.State == "APPROVED" {
			continue
		}
		latest[key] = review
	}
	return latest, postMergeConcern
}

// reviewerKey returns a stable per-reviewer identity for map deduplication.
// Prefers the immutable numeric ReviewerID (immune to login renames); falls
// back to lowercased login for legacy data where ReviewerID is zero.
func reviewerKey(r model.Review) string {
	return fmt.Sprintf("id:%d", r.ReviewerID)
}

// evaluateRequiredChecks determines the owner approval status for a set
// of required checks against the check runs for a given commit SHA.
// Returns "success" if all pass, "failure" if any found but failed,
// "missing" if not found.
func evaluateRequiredChecks(checkRuns []model.CheckRun, headSHA string, requiredChecks []RequiredCheck) string {
	if len(requiredChecks) == 0 {
		return ""
	}
	allPassed := true
	anyFailed := false
	for _, rc := range requiredChecks {
		found := false
		for _, cr := range checkRuns {
			if cr.CommitSHA == headSHA && strings.EqualFold(cr.CheckName, rc.Name) {
				found = true
				if !strings.EqualFold(cr.Conclusion, rc.Conclusion) {
					anyFailed = true
					allPassed = false
				}
				break
			}
		}
		if !found {
			allPassed = false
		}
	}
	if allPassed {
		return "success"
	}
	if anyFailed {
		return "failure"
	}
	return "missing"
}

// isSelfApproval checks whether a review's reviewer is also a code
// contributor and therefore cannot count as an independent approval.
//
// ID-only: all identity matching uses immutable numeric GitHub account
// IDs. Returns false when ReviewerID is 0 (unresolved reviewer cannot
// be proven to be the same person). No login-string fallback — logins
// are mutable and forgery-prone.
//
// Three identity sources are tested against ReviewerID:
//
//   - PR author (AuthorID)        — always.
//   - Commit author (AuthorID)    — skipped on CleanMerge.
//   - PR-branch commit authors    — every AuthorID on the PR's commits,
//     for squash-merge coverage.
//
// Co-authored-by trailers and committer login are intentionally excluded:
// trailers are unvalidated (forgeable), and committer has no API-provided
// numeric ID on the commit object.
//
// See Architecture.md §5 for why CleanMerge exempts the author check:
// GitHub's merge button refuses to produce a CleanMerge under conflicts,
// so no author-contributed bytes can ride along.
func isSelfApproval(review model.Review, commit model.Commit, pr model.PullRequest, prBranchCommits []model.Commit) bool {
	if review.ReviewerID == 0 {
		return false
	}

	if sameUser(review.ReviewerID, pr.AuthorID) {
		return true
	}

	mergeKind := github.ClassifyMerge(commit.ParentCount, commit.Message, commit.CommitterLogin, commit.IsVerified)
	if mergeKind != github.CleanMerge {
		if sameUser(review.ReviewerID, commit.AuthorID) {
			return true
		}
	}

	for _, c := range prBranchCommits {
		if sameUser(review.ReviewerID, c.AuthorID) {
			return true
		}
	}

	return false
}

// sameUser returns true if two GitHub identities refer to the same account.
// ID-only: returns true only when both IDs are non-zero and equal. Returns
// false when either ID is missing — no identity claim without immutable proof.
func sameUser(id1, id2 int64) bool {
	return id1 != 0 && id2 != 0 && id1 == id2
}

// ----- Pure utilities -----

// truncateSHA returns the first 12 chars of a git SHA, or the full string
// if it's shorter. Used in reason strings to keep them readable.
func truncateSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// hasNonExemptPRContributors returns true if any PR branch commit author
// is not in the exempt list. Used to prevent exempt-author early return
// when a squash merge contains human contributions. Mirrors
// applyExemptAuthorRule's matching: id when present, falling back to
// `verified_emails` when AuthorID == 0.
//
// Empty commits (zero additions and zero deletions) are skipped — they
// can't introduce any code into the squash merge, so their author's
// exempt status is irrelevant to "did human code ship". This mirrors
// the empty-commit exclusion in distinctPRCommitAuthors (Architecture.md
// §5) and avoids voiding the §1 carve-out on GitHub stub commits or
// "Empty commit to rerun check" markers.
//
// PR-branch commits come from /pulls/{n}/commits which (unlike
// /repos/{o}/{r}/commits) does not go through requireAuthor — so an
// AuthorID of 0 is normal. The verified_emails fallback lets the
// operator vet service accounts whose emails GitHub doesn't bind to a
// verified account; without a match (or with no email at all) we fail
// closed, treating the contributor as non-exempt.
func hasNonExemptPRContributors(enrichment model.EnrichmentResult, exemptAuthors []model.ExemptAuthor) bool {
	if len(enrichment.PRBranchCommits) == 0 {
		return false
	}
	for _, commits := range enrichment.PRBranchCommits {
		for _, c := range commits {
			if c.Additions == 0 && c.Deletions == 0 {
				continue
			}
			if !isExemptCommit(c.AuthorID, c.AuthorEmail, exemptAuthors) {
				return true
			}
		}
	}
	return false
}

// filterNonEmptyContributors returns one representative commit per unique
// author from a PR's branch commits, keeping only authors with non-empty
// contributions. Used by isSelfApproval to compare reviewer identity
// against PR branch contributors using both AuthorID and AuthorLogin.
func filterNonEmptyContributors(org, repo string, commits []model.Commit, fetchStats StatsFetcher) []model.Commit {
	type authorGroup struct {
		representative model.Commit
		all            []model.Commit
	}
	byKey := make(map[string]*authorGroup)
	var order []string
	for _, c := range commits {
		if c.AuthorID == 0 && c.AuthorLogin == "" {
			continue
		}
		var key string
		if c.AuthorID != 0 {
			key = fmt.Sprintf("id:%d", c.AuthorID)
		} else {
			key = strings.ToLower(c.AuthorLogin)
		}
		if _, ok := byKey[key]; !ok {
			byKey[key] = &authorGroup{representative: c}
			order = append(order, key)
		}
		byKey[key].all = append(byKey[key].all, c)
	}

	var out []model.Commit
	for _, key := range order {
		g := byKey[key]
		if hasNonEmptyContribution(org, repo, g.all, fetchStats) {
			out = append(out, g.representative)
		}
	}
	return out
}

// isApprovalRefreshable implements the §4 stale-approval carve-out
// for post-approval commits produced exclusively by exempt-list
// accounts (typically CI bots performing branch-sync merges, but the
// rule is more general). Returns true iff every PR-branch commit
// committed strictly after the approval is authored by an account in
// the exempt list.
//
// The trust boundary is the exempt-author ID match. AuthorID is set
// by GitHub from the commit's git-author email's verified account
// binding — a local actor cannot forge it. The exempt list is the
// curated set of accounts the operator has already vetted as not
// requiring human review (§1); commits by those accounts after an
// approval don't invalidate the approval's coverage.
//
// One non-exempt commit between approval and merge means real
// human-authored code shipped that the reviewer never saw — the
// original §4 stale flag is the right verdict.
//
// An empty post-approval set returns false — the caller only invokes
// this on an old-SHA review, so a post-approval set should always be
// non-empty in practice; treating "no post-approval commits" as
// non-refreshable is the safe default.
//
// `mergeCommitSHA` is the PR's `merge_commit_sha`. The PR-branch list is
// built from `commit_prs ⨝ commits`, which links the squash-merge
// commit on master to the PR — so it ends up in the per-PR commit list
// even though it's the merge destination, not a branch contribution.
// We skip that SHA before applying the exempt check; otherwise a
// human-authored squash-merge commit (the normal case for a
// human-authored PR) would always void the carve-out.
func isApprovalRefreshable(approval model.Review, prBranchCommits []model.Commit, mergeCommitSHA string, exemptAuthors []model.ExemptAuthor) bool {
	if len(prBranchCommits) == 0 {
		return false
	}
	postApprovalCount := 0
	for _, c := range prBranchCommits {
		if mergeCommitSHA != "" && c.SHA == mergeCommitSHA {
			continue
		}
		if !c.CommittedAt.After(approval.SubmittedAt) {
			continue
		}
		postApprovalCount++
		if !isExemptCommit(c.AuthorID, c.AuthorEmail, exemptAuthors) {
			return false
		}
	}
	return postApprovalCount > 0
}

// distinctPRCommitAuthors returns unique author logins from a single PR's
// branch commits (for the PRCommitAuthorLogins report column).
//
// Authors whose every contribution to this PR branch is an empty commit
// (zero additions and zero deletions) are dropped. The reviewer pushing an
// "Empty commit to rerun check" or similar admin commit is the prototypical
// false positive: GitHub's /pulls/N/commits endpoint omits diff stats, so
// every commit looks zero-stat at this point. fetchStats lazily resolves
// the truth via GetCommitDetail (DB-cached), and is only invoked for an
// author whose listed contributions all *appear* empty — the common case
// where the author has any commit with non-zero stats short-circuits before
// any API call. fetchStats may be nil; without it the conservative
// pre-filter behaviour (treat the author as a contributor) is preserved.
func distinctPRCommitAuthors(org, repo string, commits []model.Commit, fetchStats StatsFetcher) []string {
	byAuthor := make(map[string][]model.Commit)
	var order []string
	for _, c := range commits {
		if c.AuthorLogin == "" {
			continue
		}
		lower := strings.ToLower(c.AuthorLogin)
		if _, ok := byAuthor[lower]; !ok {
			order = append(order, c.AuthorLogin)
		}
		byAuthor[lower] = append(byAuthor[lower], c)
	}

	var out []string
	for _, login := range order {
		if hasNonEmptyContribution(org, repo, byAuthor[strings.ToLower(login)], fetchStats) {
			out = append(out, login)
		}
	}
	return out
}

// hasNonEmptyContribution reports whether any of an author's PR-branch
// commits modified at least one line. If every commit's local stats look
// empty (typical: PR-branch commits returned by /pulls/N/commits never
// carry stats), fetchStats is consulted per commit to disambiguate
// truly-empty admin commits from un-fetched ones. A nil fetcher or any
// fetch error fails open (returns true) so we never silently drop an
// author who actually contributed.
func hasNonEmptyContribution(org, repo string, commits []model.Commit, fetchStats StatsFetcher) bool {
	for _, c := range commits {
		if c.Additions > 0 || c.Deletions > 0 {
			return true
		}
	}
	if fetchStats == nil {
		return true
	}
	for _, c := range commits {
		a, d, err := fetchStats(StatsTriggerSelfApproval, org, repo, c.SHA)
		if err != nil {
			return true
		}
		if a > 0 || d > 0 {
			return true
		}
	}
	return false
}

// distinctPRBranchAuthors returns unique author logins across every PR's
// branch commits on a commit (for the PRCommitAuthorLogins report column).
func distinctPRBranchAuthors(prBranchCommits map[int][]model.Commit) []string {
	seen := make(map[string]bool)
	var result []string
	for _, commits := range prBranchCommits {
		for _, c := range commits {
			if c.AuthorLogin != "" && !seen[strings.ToLower(c.AuthorLogin)] {
				seen[strings.ToLower(c.AuthorLogin)] = true
				result = append(result, c.AuthorLogin)
			}
		}
	}
	return result
}

// classifyMergeStrategy labels a commit as "initial", "merge", "squash",
// "rebase", or "direct-push". Informational only — does not affect compliance.
//
// Squash vs rebase: GitHub's squash merge always sets committer=web-flow with
// a verified signature. Rebase (fast-forward) preserves the original committer.
// Non-fast-forward rebases also get web-flow and are indistinguishable from
// squash at the commit level — we accept that ambiguity.
func classifyMergeStrategy(c model.Commit, hasPR bool) string {
	switch {
	case c.ParentCount == 0:
		return "initial"
	case c.ParentCount > 1:
		return "merge"
	case hasPR && strings.EqualFold(c.CommitterLogin, "web-flow"):
		return "squash"
	case hasPR:
		return "rebase"
	default:
		return "direct-push"
	}
}
