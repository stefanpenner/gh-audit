# model

Core domain types for gh-audit. See [`types.go`](types.go) for definitions.

## Type Relationships

```mermaid
classDiagram
    direction LR

    class RepoInfo {
        Org string
        Name string
        FullName string
        DefaultBranch string
        Archived bool
    }

    class SyncCursor {
        Org string
        Repo string
        Branch string
        LastDate time.Time
    }

    class CoAuthor {
        Login string
        Email string
        Name string
    }

    class Commit {
        Org string
        Repo string
        SHA string
        AuthorLogin string
        AuthorID int64
        AuthorEmail string
        CommitterLogin string
        CoAuthors []CoAuthor
        CommittedAt time.Time
        Message string
        IsVerified bool
        ParentCount int
        Additions int
        Deletions int
        Branch string
        Href string
    }

    class PullRequest {
        Org string
        Repo string
        Number int
        Title string
        Merged bool
        HeadSHA string
        MergeCommitSHA string
        AuthorLogin string
        AuthorID int64
        MergedByLogin string
        MergedByID int64
        MergedAt time.Time
        Href string
    }

    class Review {
        Org string
        Repo string
        PRNumber int
        ReviewID int64
        ReviewerLogin string
        ReviewerID int64
        State string
        CommitID string
        SubmittedAt time.Time
        Href string
    }

    class CheckRun {
        Org string
        Repo string
        CommitSHA string
        CheckRunID int64
        CheckName string
        Status string
        Conclusion string
        CompletedAt time.Time
    }

    class EnrichmentResult {
        Commit Commit
        PRs []PullRequest
        Reviews []Review
        CheckRuns []CheckRun
    }

    class AuditResult {
        Org string
        Repo string
        SHA string
        IsCompliant bool
        IsBot bool
        IsExemptAuthor bool
        IsEmptyCommit bool
        IsSelfApproved bool
        HasFinalApproval bool
        HasStaleApproval bool
        HasPostMergeConcern bool
        HasPR bool
        PRNumber int
        PRCount int
        IsCleanRevert bool
        RevertVerification string
        IsCleanMerge bool
        MergeVerification string
        MergeStrategy string
        ApproverLogins []string
        Reasons []string
        Annotations []string
    }

    RepoInfo "1" --o "*" Commit : contains
    RepoInfo "1" --o "1" SyncCursor : tracks
    Commit "*" --o "*" PullRequest : associated via
    PullRequest "1" --o "*" Review : has
    Commit "1" --o "*" CheckRun : has
    EnrichmentResult "1" --> "1" Commit : wraps
    EnrichmentResult "1" --> "*" PullRequest
    EnrichmentResult "1" --> "*" Review
    EnrichmentResult "1" --> "*" CheckRun
    Commit "1" --> "*" CoAuthor : has
    Commit "1" --> "1" AuditResult : produces
```
