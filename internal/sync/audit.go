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

// EvaluateCommit determines compliance for a single commit by running the
// audit rule list documented in Architecture.md §1–§8. The function body
// deliberately reads as that rule list top-to-bottom; each rule delegates to
// a small helper whose name points back to the Architecture section.
//
// Performance notes:
//   - The empty-commit fallback (§2) is evaluated LAST on commit-with-PRs
//     paths and only after rules 4–6 fail. Eager prefetching of
//     additions/deletions previously accounted for ~16% of REST traffic on
//     full sweeps; fetchStats lets the fallback resolve stats lazily on
//     already-suspect commits only. A nil fetchStats is tolerated — the
//     fallback then uses the caller-supplied stats as-is.
//   - Rule 8 (clean-revert waiver) is standalone: it judges the revert on
//     its own signals and does not inspect the reverted commit's verdict.
//     See TODO.md for the stricter cross-commit variant.
func EvaluateCommit(commit model.Commit, enrichment model.EnrichmentResult, exemptAuthors []string, requiredChecks []RequiredCheck, fetchStats StatsFetcher) model.AuditResult {
	// Informational fields shared by every return path (revert/merge
	// signals, annotations, IsBot). Populated once, before any rule runs.
	result := initAuditResult(commit, enrichment)

	// Rule 1 — Exempt author.
	if applyExemptAuthorRule(&result, commit, enrichment, exemptAuthors) {
		return result
	}

	// Rule 3 — Has associated PR.
	if len(enrichment.PRs) == 0 {
		result.HasPR = false
		// Rule 2 — Empty commit (no-PR path).
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

	// Rules 4, 5, 6 — per-PR evaluation (approval on final commit, self-
	// approval exclusion, required status checks). Rule 7 short-circuits
	// the loop as soon as any PR satisfies both rule 4 and rule 6.
	var best prVerdict
	for i := range enrichment.PRs {
		v := evaluatePR(commit, enrichment, &enrichment.PRs[i], requiredChecks)
		if v.approvalOnFinal && v.ownerApprovalOK {
			return finalizeCompliantPR(result, commit, enrichment, v)
		}
		if betterVerdict(v, best) {
			best = v
		}
	}

	// Rule 2 — Empty commit (post-PR-loop path). A PR that never modified
	// bytes is trivially compliant regardless of review state.
	if applyEmptyCommitFallback(&result, &commit, fetchStats) {
		return result
	}

	// Rule 8 — Clean-revert waiver (standalone).
	if ok, reason := evaluateRevertCompliance(commit, enrichment); ok {
		return finalizeRevertWaiver(result, commit, enrichment, best, reason)
	}

	// Rule 7 — Non-compliant verdict, reporting the best PR's reasons.
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

// applyExemptAuthorRule implements Architecture.md §1. Returns true iff the
// commit is waived as exempt (caller should return result as-is). When the
// commit author matches but the PR contains non-exempt contributors (squash
// merge with human work), the exemption is NOT granted — IsExemptAuthor is
// still marked so reviewers can see the match — and the function returns
// false so downstream rules audit the human code.
func applyExemptAuthorRule(result *model.AuditResult, commit model.Commit, enrichment model.EnrichmentResult, exemptAuthors []string) bool {
	for _, exempt := range exemptAuthors {
		if !strings.EqualFold(commit.AuthorLogin, exempt) {
			continue
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
	return false
}

// A prVerdict captures the rules 4–6 outcome for a single PR. It's both the
// early-return payload (when approvalOnFinal && ownerApprovalOK) and the
// "closest to compliant" candidate tracked across multiple PRs.
type prVerdict struct {
	pr               *model.PullRequest
	reasons          []string
	approvers        []string
	commitAuthors    []string // distinct PR-branch commit authors (for self-approval check)
	approvalOnFinal  bool     // rule 4: non-self APPROVED on pr.HeadSHA
	selfApproved     bool     // rule 5: any APPROVED whose reviewer is a code author
	staleApproval    bool     // rule 4: non-self APPROVED on older SHA
	postMergeConcern bool     // §4 cutoff: post-merge CHANGES_REQUESTED/DISMISSED
	ownerApproval    string   // rule 6: "", "success", "failure", "missing"
	ownerApprovalOK  bool     // rule 6: ownerApproval is "" or "success"
}

// evaluatePR applies Architecture.md §4 (approval on final commit), §5
// (self-approval exclusion), and §6 (required status checks) to a single PR
// and returns the resulting verdict. The function is side-effect free.
func evaluatePR(commit model.Commit, enrichment model.EnrichmentResult, pr *model.PullRequest, requiredChecks []RequiredCheck) prVerdict {
	v := prVerdict{pr: pr}
	v.commitAuthors = distinctPRCommitAuthors(enrichment.PRBranchCommits[pr.Number])

	// Rule 4 — per-reviewer latest state on pr.HeadSHA with merge-time cutoff.
	latestByReviewer, postMergeConcern := latestReviewStatesOnFinal(enrichment.Reviews, *pr)
	v.postMergeConcern = postMergeConcern

	// Rules 4+5 — classify each APPROVED review as self or independent.
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

	// Rule 4 — stale-approval detection (only meaningful when no fresh approval).
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

	// Reason string when rule 4 fails — distinguishes self-approval, stale,
	// and never-reviewed so reports point reviewers at the right concern.
	if !v.approvalOnFinal {
		switch {
		case v.selfApproved:
			v.reasons = append(v.reasons, fmt.Sprintf("self-approved (reviewer is code author) (PR #%d)", pr.Number))
		case v.staleApproval:
			v.reasons = append(v.reasons, fmt.Sprintf("approval is stale — not on final commit (PR #%d)", pr.Number))
		default:
			v.reasons = append(v.reasons, fmt.Sprintf("no approval on final commit (PR #%d)", pr.Number))
		}
	}

	// Rule 6 — required status checks.
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

// finalizeCompliantPR sets the compliant verdict from a PR that passed both
// rule 4 (non-self approval on final commit) and rule 6 (required checks).
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

// evaluateRevertCompliance returns (true, reason) iff the commit is a clean
// revert — either an AutoRevert (trusted by construction) or a ManualRevert
// whose diff was verified as the exact inverse of the reverted commit.
// Returns (false, "") otherwise.
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

// distinctPRCommitAuthors returns unique author logins from a single PR's
// branch commits (for self-approval checks scoped to that PR).
func distinctPRCommitAuthors(commits []model.Commit) []string {
	var out []string
	seen := make(map[string]bool)
	for _, c := range commits {
		if c.AuthorLogin == "" {
			continue
		}
		lower := strings.ToLower(c.AuthorLogin)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, c.AuthorLogin)
	}
	return out
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
