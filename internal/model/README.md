# model

Core domain types for gh-audit. See [`types.go`](types.go) for definitions.

## Type Relationships

```mermaid
classDiagram
    direction LR

    class RepoInfo {
        Org string
        Name string
        DefaultBranch string
        Archived bool
    }

    class SyncCursor {
        Org string
        Repo string
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
        TreeSHA string
        ParentSHAs []string
        AuthorLogin string
        AuthorEmail string
        AuthorName string
        CommitterLogin string
        CommitterEmail string
        CommitterName string
        CoAuthors []CoAuthor
        CommittedAt time.Time
        Message string
        IsVerified bool
        SignatureType string
        ParentCount int
        Additions int
        Deletions int
        IsGitHubGenerated bool
    }

    class CommitPullRequest {
        Org string
        Repo string
        SHA string
        PRNumber int
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
        MergedAt time.Time
    }

    class Review {
        Org string
        Repo string
        PRNumber int
        ReviewerLogin string
        State string
        CommitID string
        SubmittedAt time.Time
    }

    class CheckRun {
        Org string
        Repo string
        CommitSHA string
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
        HasPR bool
        HasFinalApproval bool
        ApproverLogins []string
        Reasons []string
        AuditedAt time.Time
    }

    RepoInfo "1" --o "*" Commit : contains
    RepoInfo "1" --o "1" SyncCursor : tracks
    Commit "*" --o "*" CommitPullRequest : joins
    CommitPullRequest "*" o-- "*" PullRequest : joins
    PullRequest "1" --o "*" Review : has
    Commit "1" --o "*" CheckRun : has
    EnrichmentResult "1" --> "1" Commit : wraps
    EnrichmentResult "1" --> "*" PullRequest
    EnrichmentResult "1" --> "*" Review
    EnrichmentResult "1" --> "*" CheckRun
    Commit "1" --> "*" CoAuthor : has
    Commit "1" --> "1" AuditResult : produces
```
