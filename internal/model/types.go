// Package model defines the core domain types for gh-audit.
// See README.md for a rendered UML class diagram of type relationships.
package model

import "time"

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
	SignatureType      string // gpg, ssh, smime, unsigned
	ParentCount       int
	Additions         int
	Deletions         int
	IsGitHubGenerated bool // merge commits, reverts, squashes created by GitHub
	Href              string
}

type CoAuthor struct {
	Login string
	Email string
	Name  string
}

type CommitPullRequest struct {
	Org      string
	Repo     string
	SHA      string
	PRNumber int
}

type PullRequest struct {
	Org            string
	Repo           string
	Number         int
	Title          string
	Merged         bool
	HeadSHA        string
	MergeCommitSHA string
	AuthorLogin    string
	MergedAt       time.Time
	Href           string
}

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

type CheckRun struct {
	Org         string
	Repo        string
	CommitSHA   string
	CheckRunID  int64
	CheckName   string
	Status      string // queued, in_progress, completed
	Conclusion  string // success, failure, neutral, cancelled, etc.
	CompletedAt time.Time
}

type AuditResult struct {
	Org                  string
	Repo                 string
	SHA                  string
	IsEmptyCommit        bool
	IsBot                bool
	HasPR                bool
	PRNumber             int
	HasFinalApproval     bool
	ApproverLogins       []string
	OwnerApprovalCheck   string // success, failure, missing
	IsCompliant          bool
	Reasons              []string
	CommitHref           string
	PRHref               string
	AuditedAt            time.Time
}

type SyncCursor struct {
	Org       string
	Repo      string
	LastDate  time.Time
	UpdatedAt time.Time
}

type RepoInfo struct {
	Org          string
	Name         string
	FullName     string
	DefaultBranch string
	Archived     bool
}

type EnrichmentResult struct {
	Commit       Commit
	PRs          []PullRequest
	Reviews      []Review
	CheckRuns    []CheckRun
}
