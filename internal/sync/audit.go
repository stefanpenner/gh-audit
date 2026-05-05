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

// StatsFetcher resolves a commit's additions/deletions. Used by EvaluateCommit
// for the empty-commit fallback so we only pay for GetCommitDetail when no
// other compliance path has succeeded. Implementations should check the DB
// first and fall through to the REST API; returning any error leaves the
// stats at whatever the caller passed in (typically zero).
type StatsFetcher func(org, repo, sha string) (additions, deletions int, err error)

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
		v := evaluatePR(commit, enrichment, &enrichment.PRs[i], requiredChecks, fetchStats)
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
// Matching is strict id-only — see isExempt for the full rationale.
// commit.AuthorID is guaranteed populated by ingestion (client.go's
// requireAuthor refuses commits without it), so the rule never has to
// fall back to login or other forgeable signals.
func applyExemptAuthorRule(result *model.AuditResult, commit model.Commit, enrichment model.EnrichmentResult, exemptAuthors []model.ExemptAuthor) bool {
	if !isExempt(commit.AuthorID, exemptAuthors) {
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

// isExempt is the shared id-only matcher used by §1 and the
// PR-branch-contributor scan. Numeric account IDs are the only signal
// trusted for exempt-author matching:
//
//   - GitHub-controlled and immutable per account.
//   - Never reused across deletions (unlike usernames, which return to
//     the pool after a 90-day cooldown and can be claimed by anyone).
//   - Cannot be forged client-side — the operator cannot fabricate a
//     commit whose author.id resolves to a particular account.
//
// Commit ingestion (internal/github/client.go::requireAuthor) refuses
// commits with no resolved author id and surfaces a fix-it message. By
// the time isExempt is called, every commit in scope is guaranteed to
// have AuthorID != 0; an entry whose ID is zero (the YAML schema allows
// it for un-resolved bare-string legacy entries, though those are no
// longer accepted) is silently ignored — never matches.
func isExempt(authorID int64, exemptAuthors []model.ExemptAuthor) bool {
	if authorID == 0 {
		return false
	}
	for _, e := range exemptAuthors {
		if e.ID != 0 && e.ID == authorID {
			return true
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
		if adds, dels, err := fetchStats(commit.Org, commit.Repo, commit.SHA); err == nil {
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
	commitAuthors    []string // distinct PR-branch commit authors (for self-approval check)
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
func evaluatePR(commit model.Commit, enrichment model.EnrichmentResult, pr *model.PullRequest, requiredChecks []RequiredCheck, fetchStats StatsFetcher) prVerdict {
	v := prVerdict{pr: pr}
	v.commitAuthors = distinctPRCommitAuthors(commit.Org, commit.Repo, enrichment.PRBranchCommits[pr.Number], fetchStats)

	// Phase 1 — per-reviewer latest state on pr.HeadSHA with merge-time cutoff.
	latestByReviewer, postMergeConcern := latestReviewStatesOnFinal(enrichment.Reviews, *pr)
	v.postMergeConcern = postMergeConcern
	for _, review := range latestByReviewer {
		if review.State != "APPROVED" {
			continue
		}
		if isSelfApproval(review, commit, *pr, v.commitAuthors) {
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
			if review.State == "APPROVED" && !isSelfApproval(review, commit, *pr, v.commitAuthors) {
				v.staleApproval = true
				break
			}
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
			if review.State == "APPROVED" && !isSelfApproval(review, commit, *best.pr, best.commitAuthors) {
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
		if review.PRNumber != pr.Number || review.CommitID != pr.HeadSHA || review.ReviewerLogin == "" {
			continue
		}
		if !pr.MergedAt.IsZero() && review.SubmittedAt.After(pr.MergedAt) {
			if review.State == "CHANGES_REQUESTED" || review.State == "DISMISSED" {
				postMergeConcern = true
			}
			continue
		}
		existing, exists := latest[review.ReviewerLogin]
		if !exists {
			latest[review.ReviewerLogin] = review
			continue
		}
		if !review.SubmittedAt.After(existing.SubmittedAt) {
			continue
		}
		if review.State == "COMMENTED" && existing.State == "APPROVED" {
			continue
		}
		latest[review.ReviewerLogin] = review
	}
	return latest, postMergeConcern
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
// Five identities are tested against review.ReviewerLogin:
//
//   - PR author                 — always.
//   - Commit author             — skipped on CleanMerge.
//   - Commit committer          — skipped on CleanMerge; web-flow / github
//     are ignored always.
//   - Co-authored-by trailers   — every login listed on the commit.
//   - PR-branch commit authors  — every author on the PR's commits,
//     for squash-merge coverage.
//
// See Architecture.md §5 for why CleanMerge exempts the author/committer
// check: GitHub's merge button refuses to produce a CleanMerge under
// conflicts, so no committer-authored bytes can ride along.
func isSelfApproval(review model.Review, commit model.Commit, pr model.PullRequest, prCommitAuthors []string) bool {
	reviewer := strings.ToLower(review.ReviewerLogin)

	if reviewer == "" {
		return false
	}

	if strings.EqualFold(pr.AuthorLogin, reviewer) {
		return true
	}

	mergeKind := github.ClassifyMerge(commit.ParentCount, commit.Message, commit.CommitterLogin, commit.IsVerified)
	if mergeKind != github.CleanMerge {
		if strings.EqualFold(commit.AuthorLogin, reviewer) {
			return true
		}

		committer := strings.ToLower(commit.CommitterLogin)
		if committer != "" && committer != "web-flow" && committer != "github" && committer == reviewer {
			return true
		}
	}

	for _, ca := range commit.CoAuthors {
		if strings.EqualFold(ca.Login, reviewer) {
			return true
		}
	}

	for _, author := range prCommitAuthors {
		if strings.EqualFold(author, reviewer) {
			return true
		}
	}

	return false
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
// when a squash merge contains human contributions. Strict id-only,
// matching applyExemptAuthorRule.
//
// Note: PR-branch commits come from /pulls/{n}/commits which (unlike
// /repos/{o}/{r}/commits) does not go through requireAuthor. A
// PR-branch commit with AuthorID == 0 means GitHub couldn't bind the
// branch commit's git author email to a verified account. We treat
// that as "non-exempt contributor present" — fail closed — because
// the bot exempt path is meant to short-circuit "no human code", and
// an unverifiable contributor cannot be presumed exempt.
func hasNonExemptPRContributors(enrichment model.EnrichmentResult, exemptAuthors []model.ExemptAuthor) bool {
	if len(enrichment.PRBranchCommits) == 0 {
		return false
	}
	for _, commits := range enrichment.PRBranchCommits {
		for _, c := range commits {
			if !isExempt(c.AuthorID, exemptAuthors) {
				return true
			}
		}
	}
	return false
}

// distinctPRCommitAuthors returns unique author logins from a single PR's
// branch commits (for self-approval checks scoped to that PR).
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
		a, d, err := fetchStats(org, repo, c.SHA)
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
