package sync

import (
	"fmt"
	"strings"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// RequiredCheck describes a status check that must pass for compliance.
type RequiredCheck struct {
	Name       string
	Conclusion string
}

// EvaluateCommit determines compliance for a single commit given its enrichment data.
func EvaluateCommit(commit model.Commit, enrichment model.EnrichmentResult, exemptAuthors []string, requiredChecks []RequiredCheck) model.AuditResult {
	result := model.AuditResult{
		Org:        commit.Org,
		Repo:       commit.Repo,
		SHA:        commit.SHA,
		CommitHref: commit.Href,
	}

	// Detect bot authors (informational — login ending in [bot])
	if strings.HasSuffix(strings.ToLower(commit.AuthorLogin), "[bot]") {
		result.IsBot = true
	}

	// Check exempt author list (compliance — skips review requirements)
	for _, exempt := range exemptAuthors {
		if strings.EqualFold(commit.AuthorLogin, exempt) {
			result.IsExemptAuthor = true
			result.IsCompliant = true
			result.Reasons = []string{"exempt: configured author"}
			result.MergeStrategy = classifyMergeStrategy(commit, false)
			return result
		}
	}

	// Check empty commit
	if commit.Additions == 0 && commit.Deletions == 0 {
		result.IsEmptyCommit = true
		result.IsCompliant = true
		result.Reasons = []string{"empty commit"}
		result.MergeStrategy = classifyMergeStrategy(commit, false)
		return result
	}

	// Check for associated PRs
	if len(enrichment.PRs) == 0 {
		result.HasPR = false
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

	for i := range enrichment.PRs {
		pr := &enrichment.PRs[i]
		prReasons := []string{}
		prApprovers := []string{}

		// Per-reviewer last-state tracking: for each reviewer on the final commit,
		// keep only their most recent review. A DISMISSED review overrides an
		// earlier APPROVED from the same reviewer (SOX requirement).
		latestByReviewer := make(map[string]model.Review)
		for _, review := range enrichment.Reviews {
			if review.PRNumber != pr.Number || review.CommitID != pr.HeadSHA {
				continue
			}
			if review.ReviewerLogin == "" {
				continue
			}
			existing, exists := latestByReviewer[review.ReviewerLogin]
			if !exists || review.SubmittedAt.After(existing.SubmittedAt) {
				latestByReviewer[review.ReviewerLogin] = review
			}
		}

		hasApprovalOnFinal := false
		hasSelfApproval := false
		for _, review := range latestByReviewer {
			if review.State == "APPROVED" {
				if isSelfApproval(review, commit, *pr) {
					hasSelfApproval = true
				} else {
					hasApprovalOnFinal = true
					prApprovers = append(prApprovers, review.ReviewerLogin)
				}
			}
		}

		// Detect stale approvals: approvals on older commits (pre-force-push)
		hasStaleApproval := false
		if !hasApprovalOnFinal {
			for _, review := range enrichment.Reviews {
				if review.PRNumber != pr.Number || review.CommitID == pr.HeadSHA {
					continue
				}
				if review.State == "APPROVED" && !isSelfApproval(review, commit, *pr) {
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

		if ownerApprovalStatus != "success" {
			prReasons = append(prReasons, fmt.Sprintf("Owner Approval check missing/failed (PR #%d)", pr.Number))
		}

		// If this PR satisfies all checks, commit is compliant
		if hasApprovalOnFinal && ownerApprovalStatus == "success" {
			result.IsCompliant = true
			result.HasFinalApproval = true
			result.ApproverLogins = prApprovers
			result.OwnerApprovalCheck = ownerApprovalStatus
			result.PRNumber = pr.Number
			result.PRHref = pr.Href
			result.Reasons = []string{"compliant"}
			result.MergeStrategy = classifyMergeStrategy(commit, true)
			return result
		}

		// Track best PR (fewest reasons = closest to compliant)
		if bestPR == nil || len(prReasons) < len(bestReasons) {
			bestPR = pr
			bestReasons = prReasons
			bestApprovers = prApprovers
			bestSelfApproved = hasSelfApproval && !hasApprovalOnFinal
			bestStaleApproval = hasStaleApproval
		}
	}

	// No PR satisfied all checks
	result.IsCompliant = false
	result.IsSelfApproved = bestSelfApproved
	result.HasStaleApproval = bestStaleApproval
	if bestPR != nil {
		result.PRNumber = bestPR.Number
		result.PRHref = bestPR.Href
		result.ApproverLogins = bestApprovers
		// Use per-reviewer last-state tracking for fallback too
		fallbackLatest := make(map[string]model.Review)
		for _, review := range enrichment.Reviews {
			if review.PRNumber != bestPR.Number || review.CommitID != bestPR.HeadSHA || review.ReviewerLogin == "" {
				continue
			}
			existing, exists := fallbackLatest[review.ReviewerLogin]
			if !exists || review.SubmittedAt.After(existing.SubmittedAt) {
				fallbackLatest[review.ReviewerLogin] = review
			}
		}
		for _, review := range fallbackLatest {
			if review.State == "APPROVED" && !isSelfApproval(review, commit, *bestPR) {
				result.HasFinalApproval = true
				break
			}
		}
		ownerApprovalStatus := evaluateRequiredChecks(enrichment.CheckRuns, bestPR.HeadSHA, requiredChecks)
		result.OwnerApprovalCheck = ownerApprovalStatus
	}
	result.Reasons = bestReasons
	result.MergeStrategy = classifyMergeStrategy(commit, true)
	return result
}

// evaluateRequiredChecks determines the owner approval status for a set of
// required checks against the check runs for a given commit SHA.
// Returns "success" if all pass, "failure" if any found but failed, "missing" if not found.
func evaluateRequiredChecks(checkRuns []model.CheckRun, headSHA string, requiredChecks []RequiredCheck) string {
	if len(requiredChecks) == 0 {
		return "success"
	}
	allPassed := true
	anyFailed := false
	for _, rc := range requiredChecks {
		found := false
		for _, cr := range checkRuns {
			if cr.CommitSHA == headSHA && cr.CheckName == rc.Name {
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
func isSelfApproval(review model.Review, commit model.Commit, pr model.PullRequest) bool {
	reviewer := strings.ToLower(review.ReviewerLogin)

	// Ignore empty reviewer
	if reviewer == "" {
		return false
	}

	// Check against PR author
	if strings.EqualFold(pr.AuthorLogin, reviewer) {
		return true
	}

	// Check against commit author and committer, but skip merge commits —
	// the commit author of a merge commit is the person who clicked merge,
	// not a code contributor.
	if commit.ParentCount <= 1 {
		if strings.EqualFold(commit.AuthorLogin, reviewer) {
			return true
		}

		committer := strings.ToLower(commit.CommitterLogin)
		if committer != "" && committer != "web-flow" && committer != "github" && committer == reviewer {
			return true
		}
	}

	// Check against co-authors
	for _, ca := range commit.CoAuthors {
		if strings.EqualFold(ca.Login, reviewer) {
			return true
		}
	}

	return false
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
