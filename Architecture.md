# Architecture

## What gh-audit detects

For every commit on a protected branch, gh-audit evaluates a decision tree (in order). Rules 1–6 are the primary check; rules 7–8 are fallbacks that can still flip a non-compliant verdict to compliant. A separate `HasPostMergeConcern` flag is orthogonal to compliance and tracks reviews submitted *after* merge (see rule 4).

### 1. Exempt author

If the commit author's GitHub login is in the configured `exemptions.authors` list (case-insensitive match against `commit.author_login` — not email, not display name), the commit is **compliant** immediately. No further checks run.

`author_login` is the GitHub handle (e.g. `stefanpenner`, `dependabot[bot]`) — not `author.name` or `author.email`. Empty if the commit email isn't linked to a GitHub account, in which case no exemption can match.

```
config.yaml: exemptions.authors      ← SOT: user-configured list
      │
      ▼
GET /repos/{o}/{r}/commits/{sha}
      → commit.author_login           ← SOT: GitHub REST API
      │
      ▼
EvaluateCommit (audit.go)
      → case-insensitive match?
          yes → IsCompliant=true, IsExemptAuthor=true, reason="exempt: configured author"
          no  → continue to rule 2
```

### 2. Empty commit

If `additions == 0 && deletions == 0`, the commit is **compliant** (flagged for visibility, no review required).

`applyEmptyCommitFallback` (`audit.go`) runs lazily — only on paths heading to non-compliant: once when there's no PR, and again after all PRs fail. Already-compliant commits skip the `GetCommitDetail` REST call entirely.

```
GET /repos/{o}/{r}/commits/{sha}
      → commit.additions, commit.deletions   ← SOT: GitHub REST API (commit detail)
      │
      ▼
EvaluateCommit (audit.go)
      → additions == 0 && deletions == 0?
          yes → IsCompliant=true, IsEmptyCommit=true, reason="empty commit"
          no  → continue to rule 3
```

### 3. Has associated PR

If the commit has no merged PR (direct push), it is **non-compliant** with reason `no associated pull request`.

```
GET /repos/{o}/{r}/commits/{sha}/pulls
      → []PullRequest (merged only)           ← SOT: GitHub REST API
      │
      ▼
EvaluateCommit (audit.go)
      → len(PRs) == 0?
          yes → IsCompliant=false, HasPR=false, reason="no associated pull request"
          no  → PRCount=len(PRs), continue to rule 4
```

### 4. Approval on final commit

For each associated merged PR, gh-audit builds a per-reviewer state map on the PR's head SHA. Only reviews targeting the final commit count.

Per-reviewer resolution: if the same reviewer submits multiple reviews on the final commit, only the latest state-changing review wins. A `DISMISSED` or `CHANGES_REQUESTED` at 11:00 overrides an `APPROVED` at 10:00. A later plain `COMMENTED` review does **not** clobber an earlier `APPROVED` from the same reviewer — matching GitHub's UI, where commenting after approving leaves the approval intact.

**Post-merge cutoff.** Reviews submitted after `pr.merged_at` are excluded from compliance. A post-merge `DISMISSED` or `CHANGES_REQUESTED` instead sets `HasPostMergeConcern=true` so auditors can review the concern on the dedicated XLSX sheet without the commit itself flipping state.

```
GET /repos/{o}/{r}/pulls/{n}/reviews
      → []Review (reviewer_login, state, commit_id, submitted_at)   ← SOT: GitHub REST API
      │
      ▼
Filter: review.commit_id == pr.head_sha?
      │                          │
      yes (on final commit)      no (stale)
      │                          │
      ▼                          ▼
Per-reviewer latest state    Stale approval check:
map (by submitted_at)        any APPROVED on older SHA
      │                      from non-self reviewer?
      ▼                          │
Any APPROVED (non-self)?     yes → HasStaleApproval=true
      │                          reason="approval is stale —
      yes → continue to          not on final commit"
            rule 5/6         no  → reason="no approval on
      no  → check stale ────→    final commit"
```

**Stale approval detection**: When no approval exists on the final commit but an approval exists on an earlier SHA, the reason is `approval is stale — not on final commit` instead of `no approval on final commit`. This distinguishes "never reviewed" from "reviewed, then code changed."

### 
A review is self-approval if the reviewer matches any of:
- PR author
- Commit author (skipped for `CleanMerge` commits — see below)
- Committer (skipped for `CleanMerge`)
- Any co-author (from `Co-authored-by:` trailers)

**CleanMerge exclusion**: A `CleanMerge` (2 parents + `Merge pull request #…` message + `web-flow` committer + verified GitHub signature — see [ClassifyMerge](#classifymerge-internalgithubmergego)) cannot contain committer-authored code. GitHub's merge button refuses to produce one when there are conflicts, and the verified `web-flow` signature can't be forged locally. For these commits the author/committer is just "who clicked merge," so skipping the author/committer check avoids false positives. `DirtyMerge` (any 2-parent merge missing one of those signals) and `OctopusMerge` (3+ parents) may contain conflict-resolution or edits authored by the committer, so the check still runs.

If the only approvals are self-approvals, the commit is **non-compliant**.

```
review.reviewer_login               ← SOT: GitHub REST API (reviews)
      │
      ▼
isSelfApproval (audit.go) checks against four identities:
      │
      ├── pr.author_login            ← SOT: GET /commits/{sha}/pulls
      ├── commit.author_login        ← SOT: GET /commits/{sha} (skip if CleanMerge)
      ├── commit.committer_login     ← SOT: GET /commits/{sha} (skip "web-flow", "github"; skip if CleanMerge)
      └── commit.co_authors[].login  ← SOT: co_authors table (persisted from "Co-authored-by:" trailers)
      │
      ▼
All approvals are self?
      yes → IsSelfApproved=true, reason="self-approved (reviewer is code author)"
      no  → at least one independent approval exists, continue to rule 6
```

### 6. Required status checks

Configured checks (e.g. `Owner Approval`) must appear on the PR's head SHA with the expected conclusion. Missing or failed checks make the commit **non-compliant**.

```
config.yaml: audit_rules.required_checks   ← SOT: user-configured list
      │
      ▼
GET /repos/{o}/{r}/commits/{head_sha}/check-runs
      → []CheckRun (check_name, conclusion)   ← SOT: GitHub REST API
      │
      ▼
evaluateRequiredChecks (audit.go)
      → for each required check:
          found with matching conclusion? → "success"
          found with wrong conclusion?   → "failure"
          not found?                     → "missing"
      │
      ▼
All "success"?
      yes → continue to verdict
      no  → reason="Owner Approval check missing/failed"
```

### 7. Compliance verdict

A commit is **compliant** if at least one associated PR has:
- A non-self approval on the final commit, AND
- All required checks passed

If multiple PRs exist for a commit, gh-audit picks the one closest to compliant for reporting. The total number of associated PRs is recorded (`pr_count`) and commits with `pr_count > 1` appear in the dedicated "Multiple PRs" report sheet.

### 8. Clean-revert waiver (standalone)

If the primary verdict above is **non-compliant**, one last check runs. It is evaluated per-commit — it does not look at the reverted commit's audit verdict (see `TODO.md` for the deferred cross-commit variant).

A `IsCleanRevert=true` commit is **compliant**. The signal means one of:
- `AutoRevert` — bot-generated, trusted by construction.
- `ManualRevert` whose diff was verified as the exact inverse of the reverted commit (`revert_verification = "diff-verified"`).

Every other revert shape — conflict-resolved (`diff-mismatch`), message-only, revert-of-revert, hand-crafted — falls through to the normal PR-approval rules. Provenance signals like `committer == web-flow` or a verified signature are **not** sufficient on their own: if the diff isn't a pure inverse, there are bytes on master that weren't there before, and those bytes deserve review.

```
non-compliant verdict from rules 1–7
      │
      ▼
IsCleanRevert == true?
      │
      yes ──▶ IsCompliant=true, reason="clean revert of <sha12>"
      no  ──▶ stay non-compliant (PR-approval reasons preserved)
```

```
EvaluateCommit (audit.go) — final decision:
      │
      ▼
For each associated PR:
      has non-self approval on final commit?
      AND all required checks passed?
          yes → IsCompliant=true, reason="compliant", return early
          no  → track as candidate (fewest reasons = closest to compliant)
      │
      ▼
No PR satisfied all checks:
      → IsCompliant=false
      → report best PR's reasons
      → set IsSelfApproved, HasStaleApproval flags
      │
      ▼
Write to audit_results table → surface in report
```

## Data flow

```
GitHub REST API
      │
      ▼
┌────────────-─┐     ┌───────────┐     ┌──────────┐     ┌──────────┐
│  Token Pool  │────▶│  REST     │────▶│  Sync    │────▶│  DuckDB  │
│  (rate-limit │     │  Client   │     │ Pipeline │     │          │
│   aware)     │     └───────────┘     └──────────┘     └──────────┘
└─────────────-┘                             │                │
                                             ▼                ▼
                                     ┌────────────┐   ┌────────────┐
                                     │  Audit     │   │  Report    │
                                     │  Evaluator │   │  (table,   │
                                     │            │   │  csv,json, │
                                     └────────────┘   │  xlsx)     │
                                                      └────────────┘
```

## Sync pipeline

The sync pipeline runs per-repo, per-branch. Repos sync in parallel (bounded by `concurrency`). Each branch follows these phases:

### Phase 1: Fetch commits

```
ListCommits(org, repo, branch, since, until)
  │
  ▼
UpsertCommits ──▶ commits table
UpsertCommitBranches ──▶ commit_branches table
```

The `since` date comes from (in priority order):
1. `--since` CLI flag
2. Stored sync cursor for this org/repo/branch
3. `initial_lookback_days` config (default 90)

**`commit_branches` column provenance:**

| Column | Source |
|--------|--------|
| `org` | YAML config — the organisation key under `orgs:` |
| `repo` | YAML config repos list, or auto-discovered via `GET /orgs/{org}/repos` |
| `sha` | Each commit's SHA returned by `GET /repos/{o}/{r}/commits?sha={branch}&since=…&until=…` (fully paginated) |
| `branch` | YAML config `branches:` list for the org; falls back to the repo's default branch if unset |

### Phase 2: Enrich

For each unaudited commit (no row in `audit_results`), the enricher calls five REST endpoints:

```
commit SHA
  │
  ├──▶ GET /repos/{o}/{r}/commits/{sha}
  │      → additions, deletions, co-authors
  │
  ├──▶ GET /repos/{o}/{r}/commits/{sha}/pulls
  │      → merged PRs (number, head_sha, author)
  │
  ├──▶ GET /repos/{o}/{r}/pulls/{n}             (per PR)
  │      → merged_by, full head_sha (backfills fields missing from /commits/{sha}/pulls)
  │
  ├──�� GET /repos/{o}/{r}/pulls/{n}/reviews     (per PR)
  │      → reviewer, state, commit_id, submitted_at
  │
  ├──▶ GET /repos/{o}/{r}/commits/{head}/check-runs  (per PR head SHA)
  │      → check name, conclusion
  │
  ├──▶ GET /repos/{o}/{r}/pulls/{n}/commits          (per PR, for self-approval expansion)
  │      → distinct PR-branch commit authors
  │
  └──▶ GET /repos/{o}/{r}/commits/{sha}              (only for revert classification)
         → file diffs for clean-revert verification (ManualRevert only)
```

Enrichment runs in parallel batches (25 commits/batch, bounded by `enrich_concurrency`). Inside a batch, commits are enriched concurrently (bounded by `enrichCommitFanout`, default 10), and PRs within a single commit are fetched concurrently as well (bounded by `enrichPRFanout`, default 5). All REST endpoints are fully paginated — no silent truncation.

Enrichment goes through `CachingEnricher` (see [Caching layer](#caching-layer)), which resolves many of these calls from the DB instead of the API and tracks per-endpoint hit/miss counts in `APIStats`.

Results are deduplicated by primary key before writing:

```
UpsertPullRequests ──▶ pull_requests table
UpsertReviews ──▶ reviews table
UpsertCheckRuns ──▶ check_runs table
UpsertCommitPRs ──▶ commit_prs table
```

### Phase 3: Audit

Each unaudited commit is evaluated by `EvaluateCommit()` using the enrichment data. Results are written to `audit_results`.

### Phase 4: Cursor update

The sync cursor is updated to the latest commit date, so the next sync picks up where this one left off.

## Database schema

DuckDB with 9 tables:

| Table | Primary Key | Purpose |
|---|---|---|
| `sync_cursors` | (org, repo, branch) | Incremental sync progress |
| `commits` | (org, repo, sha) | Git commits from GitHub |
| `co_authors` | (org, repo, sha, email) | Co-authors parsed from "Co-authored-by:" trailers |
| `commit_branches` | (org, repo, sha, branch) | Which branches a commit appears on |
| `commit_prs` | (org, repo, sha, pr_number) | Commit → PR associations |
| `pull_requests` | (org, repo, number) | GitHub pull requests |
| `reviews` | (org, repo, pr_number, review_id) | PR reviews with per-reviewer state |
| `check_runs` | (org, repo, commit_sha, check_run_id) | CI/CD check results |
| `audit_results` | (org, repo, sha) | Compliance verdicts with reasons |

Bulk writes use the DuckDB Appender API (staging table → INSERT OR REPLACE). All writes go through a serialized `DBWriter` — DuckDB supports concurrent reads but single-writer.

## Token pool

The token pool manages a heterogeneous set of GitHub credentials. Two token kinds are supported:

| Kind | Config fields | Auth mechanism |
|------|---------------|----------------|
| **PAT** (`kind: pat`) | `env` (env var name) | Bearer token header |
| **App** (`kind: app`) | `app_id`, `installation_id`, `private_key_path` or `private_key_env` | JWT → installation access token via [ghinstallation](https://github.com/bradleyfalzon/ghinstallation); auto-refreshes before expiry |

Each token carries a list of **scopes** (`org` + optional `repos`) that restrict which org/repo pairs it may be used for. Scope matching is case-insensitive; an empty repos list means all repos in that org.

Auto-detection fallback (when no tokens are configured): `GH_TOKEN` → `GITHUB_TOKEN` → `gh auth token`. The first found is added as a wildcard token with no scope restriction.

### Required GitHub permissions

gh-audit is read-only. The minimum permissions for each token:

| Permission | Scope | Endpoints |
|---|---|---|
| **Contents** | Read | `GET /repos/{o}/{r}/commits`, `GET /repos/{o}/{r}/commits/{sha}` |
| **Pull requests** | Read | `GET /repos/{o}/{r}/commits/{sha}/pulls`, `GET /repos/{o}/{r}/pulls/{n}/reviews` |
| **Checks** | Read | `GET /repos/{o}/{r}/commits/{ref}/check-runs` |
| **Metadata** | Read | `GET /repos/{o}/{r}`, `GET /orgs/{org}/repos` |

Classic PAT: the `repo` scope covers all of the above. Fine-grained PAT or GitHub App: enable Contents, Pull requests, Checks, and Metadata — all read-only.

### Rate limit handling

- Tracks `x-ratelimit-remaining` and `x-ratelimit-reset` from response headers
- Scores each token by `rateRemaining - inFlight` and picks the highest; the in-flight counter prevents concurrent `Pick` calls from herding onto a single token before any response has landed
- Blocks and waits for reset when all matching tokens are exhausted (threshold: 100 remaining)
- Retries on 429 (respects `Retry-After`, defaults to 60s)
- Detects 403 abuse/secondary rate limit responses
- Disables tokens permanently on 401

## Caching layer

Enrichment goes through `CachingEnricher` (`internal/github/caching.go`), which sits between the sync pipeline and the raw REST `Client`. It exists to keep enrichment idempotent and cheap: running `sync` again, or `re-audit`, or `backfill-missing-prs` should not re-fetch data already on disk.

```
enrich(sha)
  │
  ▼
in-memory map (per-run)        ── hit ──▶ APIStats.CacheHits++
  │ miss
  ▼
DB (commits, pull_requests,    ── hit ──▶ APIStats.DBHits++
     reviews, check_runs,                 + populate in-memory map
     commit_prs, co_authors)
  │ miss
  ▼
REST Client ── hit ──▶ per-endpoint APIStats counter
               (CommitDetail / CommitPRs / PRDetail / Reviews /
                CheckRuns / PRCommits / RevertVerification)
```

Key design points:
- **Reverse PR index.** A PR fetched for commit A may also be the merge PR for commit B. `indexPR` populates a reverse map so B's enrichment finds A's PR work without a second API round-trip.
- **Lazy commit detail.** `commits` written by phase 1 already carry most of what the audit needs. `GetCommitDetail` is only called when the decision tree actually needs stats (empty-commit fallback) — saving roughly 16% of REST traffic on a typical run.
- **Fan-out bounds.** `enrichCommitFanout = 10` (per batch) and `enrichPRFanout = 5` (per commit) cap goroutine growth without flooding the token pool.
- **Revert-verification telemetry.** `GetCommitFiles` calls made to diff-verify manual reverts are tracked separately in `APIStats.RevertVerification`, because they're the most expensive per-commit call and worth monitoring on their own.

## Revert & merge classification

Two small classifiers feed the audit tree and the XLSX report.

### `ParseRevert` (`internal/github/revert.go`)

| Kind | Trigger | Clean? |
|---|---|---|
| `NotRevert` | Message has no recognised revert prefix | — |
| `AutoRevert` | `Automatic revert of <new>..<old>` | **Yes** (trusted by construction) |
| `ManualRevert` | `Revert "..."` with `This reverts commit <sha>.` | **Only if** `IsCleanRevertDiff` confirms the diff is the exact inverse of the reverted commit |
| `RevertOfRevert` | Revert-of-revert (re-application) | No — treated as fresh code |

`IsCleanRevertDiff` compares file patches as multisets of added/removed lines; order is ignored. A `ManualRevert` with a failing diff check becomes `revert_verification = "diff-mismatch"` (or `"message-only"` when no trailer SHA was found) and does **not** qualify for rule 8's R1 clean-revert waiver. It may still qualify for R2 if the committer is `web-flow` and the signature is verified.

### `ClassifyMerge` (`internal/github/merge.go`)

| Kind | Parents | Extra signals |
|---|---|---|
| `NotMerge` | 0–1 | — |
| `CleanMerge` | 2 | `Merge pull request #…` message AND `committer_login == web-flow` AND `is_verified == true`. All three are required. |
| `DirtyMerge` | 2 | Any missing signal — non-matching message, non-web-flow committer, or unverified signature. Could hide committer-authored code. |
| `OctopusMerge` | 3+ | Rare; typically tooling-generated. Not auto-trusted. |

The `CleanMerge` signal is deliberately strict. Message-only matching is forgeable — anyone can craft a `Merge pull request #…` commit locally. Requiring the `web-flow` committer with a GitHub-verified signature is what makes it trustworthy: only GitHub holds the web-flow signing key, so the signal can't be produced outside GitHub's merge button.

These flags drive the **Clean Reverts** and **Clean Merges** XLSX sheets, the rule-8 fallback, and the self-approval CleanMerge exclusion (rule 5). They are **informational for compliance** except when `IsCleanRevert` is true and the reverted commit is itself compliant.

## Annotations

`internal/sync/annotations.go` computes a list of informational annotations from each commit's message. They are attached to every `audit_results` row regardless of the compliance path taken, and are **not** load-bearing for compliance today.

- `detectAutomationTag` flags automation/dep-bump markers (Dependabot, Renovate, etc.) so auditors can cross-check against exempt-author configuration.

The `annotate-commits` CLI recomputes these for every existing `audit_results` row without hitting the API — useful after adding a new detector.

## Report layer

The `report` command queries `audit_results` joined with `commits` and `pull_requests`. Four output formats:

- **table**: ASCII summary + details to stdout
- **csv**: Per-commit rows with all fields
- **json**: `{ summary: [...], details: [...] }`
- **xlsx**: 10-sheet workbook with hyperlinks, conditional formatting, and auto-filters:
  1. **Summary** — per-repo compliance rollup with counts and percentages
  2. **All Commits** — every commit with clickable SHA and PR links; an "Other PRs" column lists additional associated PR numbers with a hyperlink to the first (see the "Multiple PRs" sheet for the full breakdown)
  3. **Non-Compliant** — failures with empty Resolution column for auditor notes
  4. **Exemptions** — bots, exempt authors, empty commits
  5. **Self-Approved** — commits where the only approval came from a code contributor
  6. **Stale Approvals** — commits merged after approval became stale (force-push after review)
  7. **Post-Merge Concerns** — commits where a reviewer submitted `CHANGES_REQUESTED` or `DISMISSED` *after* merge (`HasPostMergeConcern=true`); orthogonal to compliance
  8. **Clean Reverts** — diff-verified reverts (`AutoRevert` or `ManualRevert` with `revert_verification = diff-verified`), all of which are flipped compliant by rule 8's R1 waiver
  9. **Clean Merges** — 2-parent merge commits whose message matches a recognised merge prefix (`ClassifyMerge = CleanMerge`)
  10. **Multiple PRs** — commits associated with more than one PR (one row per commit-PR pair)

Summary columns are partitioned: `Total = Compliant + Non-Compliant`. Bots, Exempt, Empty, Self-Approved, Stale Approvals, Post-Merge Concerns, Clean Reverts, Clean Merges, and Multiple PRs are cross-cutting annotations that overlap with the primary partition.

## Package structure

```
cmd/
  root.go                    Cobra root + flag wiring
  sync.go                    `sync` — fetch + enrich + audit (the main loop)
  report.go                  `report` — table / csv / json / xlsx output
  config.go                  `config` — show effective config, list tokens
  reaudit.go                 `re-audit` — re-evaluate audit_results from DB (no API, single pass)
  backfill.go                `backfill-missing-prs` — recover PR attribution for "no associated pull request" rows via time-windowed merge_commit_sha lookup
  annotate_commits.go        `annotate-commits` — recompute informational annotations on every row from commit messages (no API)
internal/
  config/                    YAML config loading, validation, defaults
  db/
    db.go                    DuckDB open, connection wiring
    schema.go                Table DDL + migrations
    appender.go              Bulk Appender-API upsert helpers
    commits.go               Commit / co-author / commit-branch queries
    prs.go                   PR, review, check-run queries
    cursor.go                Sync-cursor read/write
    audit.go                 audit_results read/write
  github/
    client.go                REST API client (all endpoints)
    tokenpool.go             Multi-token management with rate limiting
    caching.go               CachingEnricher (in-memory + DB-first fallback, APIStats, fanout)
    revert.go                ParseRevert + IsCleanRevertDiff (clean-revert detection)
    merge.go                 ClassifyMerge (CleanMerge / DirtyMerge / OctopusMerge)
  model/
    types.go                 Domain types (Commit, PR, Review, CheckRun, AuditResult, EnrichmentResult)
  report/
    report.go                Summary/detail queries, table/csv/json formatting
    xlsx.go                  10-sheet XLSX generation
  sync/
    pipeline.go              Orchestration (discover → fetch → enrich → audit)
    audit.go                 EvaluateCommit decision tree (rules 1–8)
    annotations.go           ComputeAnnotations — informational flags from commit messages
    dbwriter.go              Serialized write channel for DuckDB
    progress.go              Sync phase tracking
```

## Concurrency model

- **Repo sync**: `concurrency` goroutines via `errgroup` (default 32)
- **Enrichment**: `enrich_concurrency` batch goroutines per repo (default 16); each batch additionally fans out across commits (≤10) and PRs (≤5)
- **DB writes**: Single `DBWriter` goroutine per pipeline run — all writes serialized through a buffered channel
- **DB reads**: Safe to run concurrently (DuckDB MVCC)

## Rate limits

GitHub REST API: 5,000->15,000 requests/hour per token (PAT or App). Cost per commit: ~5 requests (detail + PRs list + PR detail + reviews + check runs). One token audits ~1,000 commits/hour. Multiple tokens multiply throughput linearly — the token pool routes requests to the least-loaded scoped token automatically. See [Token pool](#token-pool) for details.
