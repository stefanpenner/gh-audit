package sync

import (
	"fmt"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/github"
	"github.com/stefanpenner/gh-audit/internal/model"
)

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

// EvaluateCommit determines compliance for a single commit given its enrichment data.
//
// The empty-commit fallback (additions == 0 && deletions == 0 → compliant) is
// evaluated LAST, only after the PR-approval and exempt-author paths have
// failed. The eager behaviour prior to this change — fetching
// additions/deletions up-front for every commit — accounted for ~16% of all
// GitHub REST traffic on a full sweep; most commits pass the approval path
// and never need stats. fetchStats lets the fallback lazily resolve them only
// when the audit would otherwise flag the commit non-compliant.
//
// Revert waivers (see the clean-revert block below) are standalone: each
// revert is judged on its own signals (diff verification, or GitHub-server
// provenance) without looking at the reverted commit's verdict. That keeps
// the audit single-pass and order-independent. See TODO.md for the stricter
// "reverted commit must also be compliant" variant.
//
// fetchStats may be nil (tests, offline evaluations); the empty-commit
// fallback is skipped in that case and compliance uses the strict
// PR-approval path only.
func EvaluateCommit(commit model.Commit, enrichment model.EnrichmentResult, exemptAuthors []string, requiredChecks []RequiredCheck, fetchStats StatsFetcher) model.AuditResult {
	result := model.AuditResult{
		Org:        commit.Org,
		Repo:       commit.Repo,
		SHA:        commit.SHA,
		CommitHref: commit.Href,
	}

	// Clean-revert and clean-merge signals are informational and orthogonal
	// to compliance. Copy them early so every return path surfaces them.
	result.IsCleanRevert = enrichment.IsCleanRevert
	result.RevertVerification = enrichment.RevertVerification
	result.RevertedSHA = enrichment.RevertedSHA
	result.IsCleanMerge = enrichment.IsCleanMerge
	result.MergeVerification = enrichment.MergeVerification

	// Informational annotations (automation markers, etc.). Computed here
	// so they're attached to every early-return path. These do not affect
	// IsCompliant — they're metadata for reviewers.
	result.Annotations = ComputeAnnotations(commit, enrichment)

	// Detect bot authors (informational — login ending in [bot])
	if strings.HasSuffix(strings.ToLower(commit.AuthorLogin), "[bot]") {
		result.IsBot = true
	}

	// Check exempt author list (compliance — skips review requirements)
	for _, exempt := range exemptAuthors {
		if strings.EqualFold(commit.AuthorLogin, exempt) {
			result.IsExemptAuthor = true

			// For squash merges, check if PR has non-exempt contributors.
			// If so, the exempt shortcut must not apply — fall through to
			// normal review checks so the human code gets audited.
			if hasNonExemptPRContributors(enrichment, exemptAuthors) {
				break
			}

			result.IsCompliant = true
			result.Reasons = []string{"exempt: configured author"}
			result.MergeStrategy = classifyMergeStrategy(commit, false)
			return result
		}
	}

	// Check for associated PRs
	if len(enrichment.PRs) == 0 {
		result.HasPR = false
		// Empty-commit fallback: a commit with no PR is only non-compliant
		// if it actually touched files. Resolve stats lazily here so
		// approved-path commits (the majority) never trigger a REST call.
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

	// Evaluate each PR for compliance
	var bestPR *model.PullRequest
	var bestReasons []string
	var bestApprovers []string
	var bestSelfApproved bool
	var bestStaleApproval bool
	var bestPostMergeConcern bool
	var bestPRCommitAuthors []string

	for i := range enrichment.PRs {
		pr := &enrichment.PRs[i]
		prReasons := []string{}
		prApprovers := []string{}

		// Collect distinct PR commit authors for expanded self-approval checks
		var prCommitAuthors []string
		seen := make(map[string]bool)
		for _, c := range enrichment.PRBranchCommits[pr.Number] {
			if c.AuthorLogin != "" {
				lower := strings.ToLower(c.AuthorLogin)
				if !seen[lower] {
					seen[lower] = true
					prCommitAuthors = append(prCommitAuthors, c.AuthorLogin)
				}
			}
		}

		// Per-reviewer last-state tracking: for each reviewer on the final commit,
		// keep the review whose state reflects their standing at merge time.
		// Only DISMISSED or CHANGES_REQUESTED supersede an earlier APPROVED from
		// the same reviewer (SOX requirement — a withdrawn approval must not
		// count). A later COMMENTED review does NOT revoke an earlier APPROVED,
		// matching GitHub's UI behaviour where commenting after approving
		// leaves the approval intact. Reviews after MergedAt are excluded so
		// compliance reflects the point-in-time state at merge; post-merge
		// activity is tracked separately via HasPostMergeConcern.
		latestByReviewer := make(map[string]model.Review)
		hasPostMergeConcern := false
		for _, review := range enrichment.Reviews {
			if review.PRNumber != pr.Number || review.CommitID != pr.HeadSHA {
				continue
			}
			if review.ReviewerLogin == "" {
				continue
			}
			if !pr.MergedAt.IsZero() && review.SubmittedAt.After(pr.MergedAt) {
				if review.State == "CHANGES_REQUESTED" || review.State == "DISMISSED" {
					hasPostMergeConcern = true
				}
				continue
			}
			existing, exists := latestByReviewer[review.ReviewerLogin]
			if !exists {
				latestByReviewer[review.ReviewerLogin] = review
				continue
			}
			if !review.SubmittedAt.After(existing.SubmittedAt) {
				continue
			}
			// Newer review from same reviewer: only overwrite if it's a
			// state-changing review (APPROVED, CHANGES_REQUESTED, DISMISSED).
			// Plain COMMENTED reviews carry no state and must not clobber an
			// earlier APPROVED.
			if review.State == "COMMENTED" && existing.State == "APPROVED" {
				continue
			}
			latestByReviewer[review.ReviewerLogin] = review
		}

		hasApprovalOnFinal := false
		hasSelfApproval := false
		for _, review := range latestByReviewer {
			if review.State == "APPROVED" {
				if isSelfApproval(review, commit, *pr, prCommitAuthors) {
					hasSelfApproval = true
				} else {
					hasApprovalOnFinal = true
					prApprovers = append(prApprovers, review.ReviewerLogin)
				}
			}
		}

		// Detect stale approvals: approvals on an earlier SHA of this PR
		hasStaleApproval := false
		if !hasApprovalOnFinal {
			for _, review := range enrichment.Reviews {
				if review.PRNumber != pr.Number || review.CommitID == pr.HeadSHA {
					continue
				}
				if review.State == "APPROVED" && !isSelfApproval(review, commit, *pr, prCommitAuthors) {
					hasStaleApproval = true
					break
				}
			}
		}

		if !hasApprovalOnFinal {
			if hasSelfApproval {
				prReasons = append(prReasons, fmt.Sprintf("self-approved (reviewer is code author) (PR #%d)", pr.Number))
			} else if hasStaleApproval {
				prReasons = append(prReasons, fmt.Sprintf("approval is stale — not on final commit (PR #%d)", pr.Number))
			} else {
				prReasons = append(prReasons, fmt.Sprintf("no approval on final commit (PR #%d)", pr.Number))
			}
		}

		ownerApprovalStatus := evaluateRequiredChecks(enrichment.CheckRuns, pr.HeadSHA, requiredChecks)
		ownerApprovalOK := ownerApprovalStatus == "" || ownerApprovalStatus == "success"

		if !ownerApprovalOK {
			prReasons = append(prReasons, fmt.Sprintf("Owner Approval check missing/failed (PR #%d)", pr.Number))
		}

		// If this PR satisfies all checks, commit is compliant
		if hasApprovalOnFinal && ownerApprovalOK {
			result.IsCompliant = true
			result.HasFinalApproval = true
			result.HasPostMergeConcern = hasPostMergeConcern
			result.ApproverLogins = prApprovers
			result.OwnerApprovalCheck = ownerApprovalStatus
			result.PRNumber = pr.Number
			result.PRHref = pr.Href
			result.Reasons = []string{"compliant"}
			result.MergeStrategy = classifyMergeStrategy(commit, true)
			result.PRCommitAuthorLogins = distinctPRBranchAuthors(enrichment.PRBranchCommits)
			return result
		}

		// Track best PR (fewest reasons = closest to compliant; highest PR number breaks ties)
		if bestPR == nil || len(prReasons) < len(bestReasons) || (len(prReasons) == len(bestReasons) && pr.Number > bestPR.Number) {
			bestPR = pr
			bestReasons = prReasons
			bestApprovers = prApprovers
			bestSelfApproved = hasSelfApproval && !hasApprovalOnFinal
			bestStaleApproval = hasStaleApproval
			bestPostMergeConcern = hasPostMergeConcern
			bestPRCommitAuthors = prCommitAuthors
		}
	}

	// No PR satisfied all checks. Empty-commit fallback: if the commit
	// didn't actually touch any files (e.g. an empty merge/rebase artefact),
	// it's trivially compliant regardless of PR review state. Resolve stats
	// lazily; the approved-PR branch above never reaches this point, so we
	// only pay the GetCommitDetail cost on already-suspect commits.
	if applyEmptyCommitFallback(&result, &commit, fetchStats) {
		return result
	}
	// Revert waivers — each evaluated standalone (no cross-commit lookup).
	// See TODO.md for the stricter "reverted commit must also be compliant"
	// variant that we've deferred.
	//
	// R1 — clean revert. IsCleanRevert == true means one of:
	//   - AutoRevert (bot-generated, trusted by construction), OR
	//   - ManualRevert whose diff was verified as the exact inverse of the
	//     reverted commit (revert_verification == "diff-verified").
	// A clean revert puts bytes back that were already on master, so it
	// needs no fresh review.
	//
	// R2 — GitHub-server-created revert. Any revert-prefixed commit whose
	// committer is `web-flow` AND signature is GitHub-verified came through
	// the "Revert" button on github.com: only GitHub's server can produce a
	// web-flow-signed commit. Provenance substitutes for a diff match, so
	// this also covers the conflict-resolved revert case where R1 fails.
	if revertCompliant, reason := evaluateRevertCompliance(commit, enrichment); revertCompliant {
		result.IsCompliant = true
		result.HasFinalApproval = false
		if bestPR != nil {
			result.PRNumber = bestPR.Number
			result.PRHref = bestPR.Href
		}
		result.Reasons = []string{reason}
		result.MergeStrategy = classifyMergeStrategy(commit, bestPR != nil)
		result.PRCommitAuthorLogins = distinctPRBranchAuthors(enrichment.PRBranchCommits)
		return result
	}
	result.IsCompliant = false
	result.IsSelfApproved = bestSelfApproved
	result.HasStaleApproval = bestStaleApproval
	result.HasPostMergeConcern = bestPostMergeConcern
	if bestPR != nil {
		result.PRNumber = bestPR.Number
		result.PRHref = bestPR.Href
		result.ApproverLogins = bestApprovers
		// Use per-reviewer last-state tracking for fallback too, with the same
		// merged-at cutoff used in the per-PR loop.
		fallbackLatest := make(map[string]model.Review)
		for _, review := range enrichment.Reviews {
			if review.PRNumber != bestPR.Number || review.CommitID != bestPR.HeadSHA || review.ReviewerLogin == "" {
				continue
			}
			if !bestPR.MergedAt.IsZero() && review.SubmittedAt.After(bestPR.MergedAt) {
				continue
			}
			existing, exists := fallbackLatest[review.ReviewerLogin]
			if !exists {
				fallbackLatest[review.ReviewerLogin] = review
				continue
			}
			if !review.SubmittedAt.After(existing.SubmittedAt) {
				continue
			}
			if review.State == "COMMENTED" && existing.State == "APPROVED" {
				continue
			}
			fallbackLatest[review.ReviewerLogin] = review
		}
		for _, review := range fallbackLatest {
			if review.State == "APPROVED" && !isSelfApproval(review, commit, *bestPR, bestPRCommitAuthors) {
				result.HasFinalApproval = true
				break
			}
		}
		ownerApprovalStatus := evaluateRequiredChecks(enrichment.CheckRuns, bestPR.HeadSHA, requiredChecks)
		result.OwnerApprovalCheck = ownerApprovalStatus
	}
	result.Reasons = bestReasons
	result.MergeStrategy = classifyMergeStrategy(commit, true)
	result.PRCommitAuthorLogins = distinctPRBranchAuthors(enrichment.PRBranchCommits)
	return result
}

// webFlowCommitter is GitHub's server-side committer login for commits
// produced by UI actions (merge button, Revert button, edit-in-browser).
// Paired with a verified signature it's a strong provenance guard — only
// GitHub holds the web-flow signing key.
const webFlowCommitter = "web-flow"

// evaluateRevertCompliance returns (true, reason) iff the commit qualifies
// for one of the revert waivers:
//
//	R1 — clean revert (bot auto-revert or diff-verified manual revert).
//	R2 — revert-prefixed commit whose committer is web-flow AND whose
//	     signature is GitHub-verified (came from the Revert button on
//	     github.com; covers conflict-resolved reverts where R1 fails).
//
// Returns (false, "") otherwise. Each rule is evaluated standalone —
// the reverted commit's compliance is NOT consulted (see TODO.md for the
// stricter variant).
func evaluateRevertCompliance(commit model.Commit, enrichment model.EnrichmentResult) (bool, string) {
	if enrichment.IsCleanRevert {
		if enrichment.RevertedSHA != "" {
			return true, fmt.Sprintf("clean revert of %s", truncateSHA(enrichment.RevertedSHA))
		}
		return true, "clean revert"
	}
	kind, revertedSHA := github.ParseRevert(commit.Message)
	if kind == github.NotRevert {
		return false, ""
	}
	if !strings.EqualFold(commit.CommitterLogin, webFlowCommitter) || !commit.IsVerified {
		return false, ""
	}
	if revertedSHA != "" {
		return true, fmt.Sprintf("GitHub-server revert of %s (web-flow, verified)", truncateSHA(revertedSHA))
	}
	return true, "GitHub-server revert (web-flow, verified)"
}

// truncateSHA returns the first 12 chars of a git SHA, or the full string
// if it's shorter. Used in reasons strings to keep them readable.
func truncateSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// applyEmptyCommitFallback flips `result` to the "empty commit" compliant
// state when the commit's net diff is zero lines. Returns true iff the
// fallback fired (caller should return `result` unchanged).
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

// evaluateRequiredChecks determines the owner approval status for a set of
// required checks against the check runs for a given commit SHA.
// Returns "success" if all pass, "failure" if any found but failed, "missing" if not found.
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

// isSelfApproval checks whether a review's author is the same person who
// contributed code to the commit or PR. GitHub's merge bot logins
// ("web-flow", "github") are excluded from the committer check.
func isSelfApproval(review model.Review, commit model.Commit, pr model.PullRequest, prCommitAuthors []string) bool {
	reviewer := strings.ToLower(review.ReviewerLogin)

	if reviewer == "" {
		return false
	}

	if strings.EqualFold(pr.AuthorLogin, reviewer) {
		return true
	}

	// For CleanMerge commits (2 parents, auto-generated message) the commit
	// author/committer is just who clicked merge — GitHub's merge button
	// refuses to produce such a commit when there are conflicts, so no code
	// was authored here. Skip the author/committer check.
	//
	// DirtyMerge (2 parents, non-auto message) and OctopusMerge (3+ parents)
	// may carry committer-authored conflict-resolution or edits, so we still
	// check them.
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

	// For squash merges: check against all PR branch commit authors
	for _, author := range prCommitAuthors {
		if strings.EqualFold(author, reviewer) {
			return true
		}
	}

	return false
}

// hasNonExemptPRContributors returns true if any PR branch commit author is not
// in the exempt list. Used to prevent exempt-author early return when a squash
// merge contains human contributions.
func hasNonExemptPRContributors(enrichment model.EnrichmentResult, exemptAuthors []string) bool {
	if len(enrichment.PRBranchCommits) == 0 {
		return false
	}
	exemptSet := make(map[string]bool, len(exemptAuthors))
	for _, a := range exemptAuthors {
		exemptSet[strings.ToLower(a)] = true
	}
	for _, commits := range enrichment.PRBranchCommits {
		for _, c := range commits {
			if c.AuthorLogin != "" && !exemptSet[strings.ToLower(c.AuthorLogin)] {
				return true
			}
		}
	}
	return false
}

// distinctPRBranchAuthors returns unique author logins from all PR branch commits.
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

func classifyMergeStrategy(c model.Commit, hasPR bool) string {
	switch {
	case c.ParentCount == 0:
		return "initial"
	case c.ParentCount > 1:
		return "merge"
	case hasPR:
		return "squash"
	default:
		return "direct-push"
	}
}
