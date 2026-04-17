# Architecture

## What gh-audit detects

For every commit on a protected branch, gh-audit evaluates a decision tree (in order):

### 1. Exempt author

If the commit author is in the configured `exemptions.authors` list (case-insensitive match), the commit is **compliant** immediately. No further checks run.

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

If `additions == 0 && deletions == 0`, the commit is **compliant** immediately. These are flagged for auditor visibility but don't require review.

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

Per-reviewer resolution: if the same reviewer submits multiple reviews on the final commit, only the latest wins. A `DISMISSED` at 11:00 overrides an `APPROVED` at 10:00.

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

**Stale approval detection**: When no approval exists on the final commit, gh-audit checks whether approvals exist on older SHAs (pre-force-push). If found, the reason is `approval is stale — not on final commit` instead of `no approval on final commit`. This distinction helps auditors differentiate "never reviewed" from "reviewed but code changed after approval."

### 5. Self-approval detection

A review is self-approval if the reviewer matches any of:
- PR author
- Commit author
- Committer (excluding GitHub merge bots: `web-flow`, `github`)
- Any co-author (from `Co-authored-by:` trailers)

If the only approvals are self-approvals, the commit is **non-compliant**.

```
review.reviewer_login               ← SOT: GitHub REST API (reviews)
      │
      ▼
isSelfApproval (audit.go) checks against four identities:
      │
      ├── pr.author_login            ← SOT: GET /commits/{sha}/pulls
      ├── commit.author_login        ← SOT: GET /commits/{sha}
      ├── commit.committer_login     ← SOT: GET /commits/{sha} (skip "web-flow", "github")
      └── commit.co_authors[].login  ← SOT: parsed from "Co-authored-by:" trailers
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

If multiple PRs exist, gh-audit picks the one closest to compliant for reporting. The total number of associated PRs is recorded (`pr_count`) and commits with `pr_count > 1` appear in the dedicated "Multiple PRs" report sheet.

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

For each unaudited commit (no row in `audit_results`), the enricher calls four REST endpoints:

```
commit SHA
  │
  ├──▶ GET /repos/{o}/{r}/commits/{sha}
  │      → additions, deletions, co-authors
  │
  ├──▶ GET /repos/{o}/{r}/commits/{sha}/pulls
  │      → merged PRs (number, head_sha, author, merged_by)
  │
  ├──▶ GET /repos/{o}/{r}/pulls/{n}/reviews     (per PR)
  │      → reviewer, state, commit_id, submitted_at
  │
  └──▶ GET /repos/{o}/{r}/commits/{head}/check-runs  (per PR head SHA)
         → check name, conclusion
```

Enrichment runs in parallel batches (25 commits/batch, bounded by `enrich_concurrency`). All REST endpoints are fully paginated — no silent truncation.

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

DuckDB with 8 tables:

| Table | Primary Key | Purpose |
|---|---|---|
| `sync_cursors` | (org, repo, branch) | Incremental sync progress |
| `commits` | (org, repo, sha) | Git commits from GitHub |
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
- Routes each request to the scoped token with the most remaining quota
- Blocks and waits for reset when all matching tokens are exhausted (threshold: 100 remaining)
- Retries on 429 (respects `Retry-After`, defaults to 60s)
- Detects 403 abuse/secondary rate limit responses
- Disables tokens permanently on 401

## Report layer

The `report` command queries `audit_results` joined with `commits` and `pull_requests`. Four output formats:

- **table**: ASCII summary + details to stdout
- **csv**: Per-commit rows with all fields
- **json**: `{ summary: [...], details: [...] }`
- **xlsx**: 7-sheet workbook with hyperlinks, conditional formatting, and auto-filters:
  1. **Summary** — per-repo compliance rollup with counts and percentages
  2. **All Commits** — every commit with clickable SHA and PR links
  3. **Non-Compliant** — failures with empty Resolution column for auditor notes
  4. **Exemptions** — bots, exempt authors, empty commits
  5. **Self-Approved** — commits where the only approval came from a code contributor
  6. **Stale Approvals** — commits merged after approval became stale (force-push after review)
  7. **Multiple PRs** — commits associated with more than one PR (one row per commit-PR pair)

Summary columns are partitioned: `Total = Compliant + Non-Compliant`. Bots, Exempt, Empty, Self-Approved, Stale Approvals, and Multiple PRs are cross-cutting annotations that overlap with the primary partition.

## Package structure

```
cmd/                     CLI commands (sync, report, config)
internal/
  config/                YAML config loading, validation, defaults
  db/                    DuckDB schema, migrations, bulk upsert
  github/
    client.go            REST API client (all endpoints)
    tokenpool.go         Multi-token management with rate limiting
  model/
    types.go             Domain types (Commit, PR, Review, CheckRun, AuditResult)
  report/
    report.go            Summary/detail queries, table/csv/json formatting
    xlsx.go              Multi-sheet XLSX generation
  sync/
    pipeline.go          Orchestration (discover → fetch → enrich → audit)
    audit.go             EvaluateCommit decision tree
    dbwriter.go          Serialized write channel for DuckDB
    progress.go          Sync phase tracking
```

## Concurrency model

- **Repo sync**: `concurrency` goroutines via `errgroup` (default 10)
- **Enrichment**: `enrich_concurrency` goroutines per repo (default 4)
- **DB writes**: Single `DBWriter` goroutine per pipeline run — all writes serialized through a buffered channel
- **DB reads**: Safe to run concurrently (DuckDB MVCC)

## Rate limits

GitHub REST API: 5,000 requests/hour per token (PAT or App). Cost per commit: ~4 requests (detail + PRs + reviews + check runs). One token audits ~1,250 commits/hour. Multiple tokens multiply throughput linearly — the token pool routes requests to the least-loaded scoped token automatically. See [Token pool](#token-pool) for details.
