// Package model defines the core domain types for gh-audit.
// See README.md for a rendered UML class diagram of type relationships.
package model

import "time"

// A Commit is a git commit synced from GitHub.
//
// Modeled as a pure git object — knows parents, author, and committer.
// Branch and IsDefaultBranch record the ref context at sync time.
//
//	CoAuthor ←── Commit ──→ CheckRun
//	                ├──→ PullRequest (via merge commit or head SHA)
//	                ├──→ AuditResult
//	                └──→ EnrichmentResult
type Commit struct {
	Org               string
	Repo              string
	SHA               string
	TreeSHA           string
	ParentSHAs        []string
	AuthorLogin       string
	AuthorEmail       string
	AuthorName        string
	CommitterLogin    string
	CommitterEmail    string
	CommitterName     string
	CoAuthors         []CoAuthor
	CommittedAt       time.Time
	Message           string
	IsVerified        bool
	SignatureType     string // gpg, ssh, smime, unsigned
	ParentCount       int
	Additions         int
	Deletions         int
	Branch            string
	IsDefaultBranch   bool
	IsGitHubGenerated bool // merge commits, reverts, squashes created by GitHub
	Href              string
}

// A CoAuthor is an additional commit author extracted from
// "Co-authored-by" trailers in the commit message.
//
//	CoAuthor ←── Commit
type CoAuthor struct {
	Login string
	Email string
	Name  string
}

// A PullRequest is a GitHub pull request.
//
// HeadSHA is the tip of the PR's source branch — the last commit the
// author pushed. MergeCommitSHA is the commit GitHub created on the
// base branch when merged (merge, squash, or last rebase commit).
// HeadBranch is the source branch ref (e.g. "feature/xyz").
//
//	Commit ──→ PullRequest ──→ Review
//	                 └──→ EnrichmentResult
type PullRequest struct {
	Org            string
	Repo           string
	Number         int
	Title          string
	Merged         bool
	HeadSHA        string
	HeadBranch     string
	MergeCommitSHA string
	AuthorLogin    string
	MergedByLogin  string
	MergedAt       time.Time
	Href           string
}

// A Review is a GitHub pull request review (approval, request for
// changes, comment, or dismissal). CommitID records the SHA the
// reviewer saw, enabling staleness detection after force-pushes.
//
//	PullRequest ──→ Review
//	                  └──→ AuditResult
type Review struct {
	Org           string
	Repo          string
	PRNumber      int
	ReviewID      int64
	ReviewerLogin string
	State         string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	CommitID      string
	SubmittedAt   time.Time
	Href          string
}

// A CheckRun is a GitHub Actions or third-party CI check result,
// tied to a specific commit SHA.
//
//	Commit ──→ CheckRun
//	             └──→ AuditResult
type CheckRun struct {
	Org         string
	Repo        string
	CommitSHA   string
	CheckRunID  int64
	CheckName   string
	Status      string // queued, in_progress, completed
	Conclusion  string // success, failure, neutral, cancelled, skipped, timed_out, action_required
	CompletedAt time.Time
}

// An AuditResult is the compliance verdict for a single commit.
// Produced by the audit engine after evaluating a commit against
// configured rules. Primary output of the system.
//
//	Commit ──┐
//	PullRequest ──→ AuditResult ──→ report
//	Review ──┘
//	CheckRun ──┘
type AuditResult struct {
	Org                string
	Repo               string
	SHA                string
	IsEmptyCommit      bool
	IsBot              bool // author name ends with [bot] — informational only
	IsExemptAuthor     bool // author is on the configured exemption list
	HasPR              bool
	PRNumber           int
	PRCount            int
	HasFinalApproval   bool
	HasStaleApproval   bool // approval exists but on a pre-force-push commit
	IsSelfApproved     bool // true if only approvals are from code contributors
	ApproverLogins     []string
	OwnerApprovalCheck string // success, failure, missing
	IsCompliant        bool
	Reasons            []string
	MergeStrategy         string // merge, squash, direct-push, initial
	PRCommitAuthorLogins  []string
	CommitHref            string
	PRHref             string
	AuditedAt          time.Time
}

// A SyncCursor tracks incremental sync progress for a single
// repo+branch pair. On each run, only commits after LastDate are
// fetched from GitHub.
//
//	RepoInfo ──→ SyncCursor
type SyncCursor struct {
	Org       string
	Repo      string
	Branch    string
	LastDate  time.Time
	UpdatedAt time.Time
}

// A RepoInfo is a GitHub repository discovered during sync.
// Provides the metadata needed to scope sync and audit operations.
//
//	RepoInfo ──→ SyncCursor
//	    └──→ Commit
type RepoInfo struct {
	Org           string
	Name          string
	FullName      string
	DefaultBranch string
	Archived      bool
}

// An EnrichmentResult bundles a commit with all its related GitHub
// data. Intermediate structure: after fetching a commit, the enricher
// populates PRs, reviews, check runs, and PR branch commits before persisting.
//
// PRBranchCommits maps PR number → commits on that PR's feature branch.
// Used to detect all contributors in squash-merged PRs where the squash
// commit hides the original per-commit authorship.
//
//	Commit ──┐
//	PullRequest ──→ EnrichmentResult ──→ AuditResult
//	Review ──┘
//	CheckRun ──┘
type EnrichmentResult struct {
	Commit          Commit
	PRs             []PullRequest
	Reviews         []Review
	CheckRuns       []CheckRun
	PRBranchCommits map[int][]Commit // PR number → commits on the PR's branch
}
