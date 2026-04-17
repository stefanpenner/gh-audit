// Package model defines the core domain types for gh-audit.
// See README.md for a rendered UML class diagram of type relationships.
package model

import "time"

// Commit represents a single git commit synced from GitHub.
// Modeled as a pure git object: it knows its parents, author, and committer,
// but has no direct knowledge of branches or PRs.
// Depended on by: CommitPullRequest (join to PRs), CheckRun (CI results),
// AuditResult (compliance verdict), EnrichmentResult (aggregation wrapper).
// Depends on: CoAuthor (embedded co-author list).
type Commit struct {
	Org               string     // GitHub organization name
	Repo              string     // repository name (without org prefix)
	SHA               string     // full 40-char commit hash
	TreeSHA           string     // hash of the git tree object for this commit's snapshot
	ParentSHAs        []string   // SHAs of parent commits; len >1 means a merge commit
	AuthorLogin       string     // GitHub login of the author (may differ from committer)
	AuthorEmail       string     // email from the git author field
	AuthorName        string     // display name from the git author field
	CommitterLogin    string     // GitHub login of the committer (e.g. "web-flow" for GitHub merges)
	CommitterEmail    string     // email from the git committer field
	CommitterName     string     // display name from the git committer field
	CoAuthors         []CoAuthor // additional authors parsed from "Co-authored-by" trailers
	CommittedAt       time.Time  // committer timestamp (when the commit was applied)
	Message           string     // full commit message including subject and body
	IsVerified        bool       // true if GitHub verified the commit signature
	SignatureType     string     // gpg, ssh, smime, or unsigned
	ParentCount       int        // number of parents; 0=root, 1=normal, 2+=merge
	Additions         int        // total lines added across all files
	Deletions         int        // total lines removed across all files
	IsGitHubGenerated bool       // true for merge commits, reverts, and squashes created by GitHub's UI
	Href              string     // web URL to the commit on GitHub
}

// CoAuthor represents an additional commit author extracted from
// "Co-authored-by" trailers in the commit message.
// Depended on by: Commit (embedded in CoAuthors field).
// Depends on: nothing.
type CoAuthor struct {
	Login string // GitHub login, if resolvable
	Email string // email from the Co-authored-by trailer
	Name  string // display name from the Co-authored-by trailer
}

// CommitPullRequest is a join table linking commits to pull requests.
// Populated during sync via GitHub's "list PRs for a commit" API.
// Handles all merge strategies (merge, squash, rebase) and commits
// appearing in multiple PRs.
// Depended on by: AuditResult (to determine if a commit has a PR).
// Depends on: Commit (via SHA), PullRequest (via PRNumber).
type CommitPullRequest struct {
	Org      string // GitHub organization name
	Repo     string // repository name
	SHA      string // commit SHA
	PRNumber int    // pull request number this commit belongs to
}

// PullRequest represents a GitHub pull request.
// The PR owns the relationship to commits via CommitPullRequest,
// and to reviews via PRNumber.
// Depended on by: CommitPullRequest (join to commits), Review (approvals),
// EnrichmentResult (aggregation wrapper), AuditResult (compliance input).
// Depends on: nothing directly; linked to Commit via CommitPullRequest.
type PullRequest struct {
	Org            string    // GitHub organization name
	Repo           string    // repository name
	Number         int       // PR number (unique within a repo)
	Title          string    // PR title
	Merged         bool      // true if the PR was merged (not just closed)
	HeadSHA        string    // tip of the PR's source branch; the last commit the author pushed
	MergeCommitSHA string    // commit created on the base branch when merged (merge, squash, or last rebase commit)
	AuthorLogin    string    // GitHub login of the PR author
	MergedAt       time.Time // when the PR was merged; zero value if not merged
	Href           string    // web URL to the PR on GitHub
}

// Review represents a GitHub pull request review (approval, request for changes, etc.).
// Linked to a specific PR and the commit SHA the reviewer saw at review time.
// Depended on by: AuditResult (to determine approval status), EnrichmentResult.
// Depends on: PullRequest (via PRNumber), Commit (via CommitID for staleness detection).
type Review struct {
	Org           string    // GitHub organization name
	Repo          string    // repository name
	PRNumber      int       // PR this review belongs to
	ReviewID      int64     // GitHub's unique ID for this review
	ReviewerLogin string    // GitHub login of the reviewer
	State         string    // APPROVED, CHANGES_REQUESTED, COMMENTED, or DISMISSED
	CommitID      string    // SHA of the commit the review was submitted against
	SubmittedAt   time.Time // when the review was submitted
	Href          string    // web URL to the review on GitHub
}

// CheckRun represents a GitHub Actions or third-party CI check result.
// Tied to a specific commit SHA, not to a PR directly.
// Depended on by: AuditResult (required checks verification), EnrichmentResult.
// Depends on: Commit (via CommitSHA).
type CheckRun struct {
	Org         string    // GitHub organization name
	Repo        string    // repository name
	CommitSHA   string    // the commit this check ran against
	CheckRunID  int64     // GitHub's unique ID for this check run
	CheckName   string    // name of the check (e.g. "ci/build")
	Status      string    // queued, in_progress, or completed
	Conclusion  string    // success, failure, neutral, cancelled, skipped, timed_out, action_required
	CompletedAt time.Time // when the check finished; zero value if still running
}

// AuditResult is the compliance verdict for a single commit.
// Produced by the audit engine after evaluating a commit against configured rules.
// This is the primary output of the system — used for reporting and alerting.
// Depended on by: report package (output generation).
// Depends on: Commit, CommitPullRequest, PullRequest, Review, CheckRun (all as inputs).
type AuditResult struct {
	Org                string    // GitHub organization name
	Repo               string    // repository name
	SHA                string    // commit SHA being audited
	IsEmptyCommit      bool      // true if the commit has no file changes (e.g. empty merge)
	IsBot              bool      // true if the commit author is a bot account
	HasPR              bool      // true if the commit is associated with at least one PR
	PRNumber           int       // primary PR number (0 if no PR)
	HasFinalApproval   bool      // true if the PR had an approving review on the final commit
	ApproverLogins     []string  // GitHub logins of approving reviewers
	OwnerApprovalCheck string    // success, failure, or missing — whether a CODEOWNERS approval exists
	IsCompliant        bool      // overall compliance verdict
	Reasons            []string  // human-readable explanations for the compliance decision
	CommitHref         string    // web URL to the commit
	PRHref             string    // web URL to the PR (empty if no PR)
	AuditedAt          time.Time // when this audit result was computed
}

// SyncCursor tracks incremental sync progress for a single repo.
// On each sync run, only commits after LastDate are fetched from GitHub.
// Depended on by: sync package (to resume where it left off).
// Depends on: RepoInfo (one cursor per org/repo pair).
type SyncCursor struct {
	Org       string    // GitHub organization name
	Repo      string    // repository name
	LastDate  time.Time // timestamp of the most recently synced commit
	UpdatedAt time.Time // when this cursor was last written
}

// RepoInfo represents a GitHub repository discovered during sync.
// Provides the metadata needed to scope sync and audit operations.
// Depended on by: SyncCursor (one cursor per repo), Commit (org/repo scope).
// Depends on: nothing.
type RepoInfo struct {
	Org           string // GitHub organization name
	Name          string // repository name (without org prefix)
	FullName      string // "org/repo" form
	DefaultBranch string // name of the default branch (e.g. "main")
	Archived      bool   // true if the repo is archived
}

// EnrichmentResult bundles a commit with all its related GitHub data.
// Used as an intermediate structure during sync: after fetching a commit,
// the enricher populates its PRs, reviews, and check runs before persisting.
// Depended on by: audit engine (as input for producing AuditResult).
// Depends on: Commit, PullRequest, Review, CheckRun.
type EnrichmentResult struct {
	Commit    Commit        // the commit being enriched
	PRs       []PullRequest // pull requests associated with this commit
	Reviews   []Review      // reviews on those pull requests
	CheckRuns []CheckRun    // check runs for this commit
}
