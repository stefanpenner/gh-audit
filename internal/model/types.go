// Package model defines the core domain types for gh-audit.
// See README.md for a rendered UML class diagram of type relationships.
package model

import "time"

type Commit struct {
	Org            string
	Repo           string
	SHA            string
	AuthorLogin    string
	AuthorEmail    string
	CommittedAt    time.Time
	Message        string
	ParentCount    int
	Additions      int
	Deletions      int
	Href           string
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
