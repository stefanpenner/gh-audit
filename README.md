# gh-audit

Proof of Concept: GitHub commit compliance auditor. Verifies that every commit on protected branches was properly code-reviewed, approved on its final commit, and passed required status checks. Built for SOX/SOC2 audit evidence.

## What it checks

For every commit on configured branches, in this order:

1. **Author exemption** -- if the commit's author is in the exempt list **and** every PR-branch contributor is also exempt (no human code in the squash), the commit short-circuits to compliant. See [Exempt authors](#exempt-authors).
2. **Empty commits** (0 additions / 0 deletions) -- short-circuit to compliant.
3. **Has an associated merged PR** -- direct pushes are flagged.
4. **Approved on the final commit at merge time** -- stale approvals (on an earlier force-pushed commit) don't count. Reviews submitted after `pr.merged_at` are excluded (see [Point-in-time compliance](#point-in-time-compliance)).
5. **Not self-approved** -- reviewer must not be the PR author, commit author, committer, or co-author.
6. **Required checks passed** -- e.g. "Owner Approval" check ran successfully on the PR's head commit.

### Informational signals (do not change `IsCompliant`)

These flags are recorded on every audit result for separate triage — they surface governance-relevant events that reviewers may want to look at, but the compliance gate remains peer review + required checks.

- **`HasPostMergeConcern`** — a reviewer submitted a `CHANGES_REQUESTED` or `DISMISSED` review **after** the PR merged. The merge itself was compliant at the time; this captures the later concern so it isn't lost. See the `Post-Merge Concerns` XLSX sheet.
- **`IsCleanRevert` + `RevertVerification`** — classifies a commit that undoes a prior commit. Compliance policy for clean reverts is not yet codified, so they are surfaced without affecting `IsCompliant`. Verification values:
  - `none` — not a revert
  - `message-only` — bot auto-revert (`Automatic revert of <sha>..<sha>`), trusted clean by pattern; or a manual revert whose referenced commit could not be fetched
  - `diff-verified` — manual revert whose per-file patches are the exact inverse of the referenced commit's patches
  - `diff-mismatch` — manual revert that partially or incorrectly reverses the referenced commit (intervening edits, conflict resolutions, partial revert)

  Auto-reverts and diff-verified manual reverts set `IsCleanRevert = true`. See the `Clean Reverts` XLSX sheet.

### Exempt authors

The `exemptions.authors` config is a list of exact login matches (case-insensitive) that short-circuit the compliance check — **but only when every PR-branch contributor is also exempt**. This matches "bot-merged, no human code" semantics: if a service-account autobot (e.g., translation updater, dep upgrader, auto-revert) authors and merges a PR where every commit on the branch is by the same or another exempt author, no human review is required.

If even a single PR-branch commit is by a non-exempt contributor, the exempt shortcut does **not** fire and the commit falls through to the normal peer-review check. This protects against an exempt author squashing in human code unnoticed.

### Point-in-time compliance

Review states are evaluated **at merge time**, not at query time. Reviews submitted after `pr.merged_at` are excluded from the `has_approval_on_final_commit` decision. This means:

- A reviewer who approves before merge and then flips to `CHANGES_REQUESTED` afterward does **not** retroactively invalidate the merge — the merge is compliant at the time it happened, and the post-merge flip surfaces as `HasPostMergeConcern`.
- A **retroactive approval** (reviewer APPROVEs a PR days or weeks after it was already merged) is **not** counted as an at-merge approval. gh-audit flags these as non-compliant even though the PR looks "approved" in the current UI.

## Install

```
go install github.com/stefanpenner/gh-audit@latest
```

## Quick start

No config file needed. If you have `gh` CLI authenticated, just run:

```bash
# Audit a specific repo
gh-audit sync --repo my-org/my-repo

# Audit an entire org
gh-audit sync --org my-org

# Specific date range
gh-audit sync --repo my-org/my-repo --since 2026-01-01 --until 2026-04-01

# Generate a report
gh-audit report --only-failures

# Re-evaluate audit results after updating config/rules (no API calls)
gh-audit re-evaluate-commits
# (legacy alias `re-audit` still works)

# Validate your config file
gh-audit config validate

# Show resolved config
gh-audit config show
```

Token is auto-detected from: `GH_TOKEN` env, `GITHUB_TOKEN` env, then `gh auth token`.

## How the audit trace works

For each commit on a branch, gh-audit runs a REST trace to collect the evidence needed for a compliance decision and the informational signals (post-merge concern, clean revert):

```
commit (SHA)
  |
  +-- GET /repos/{owner}/{repo}/commits/{sha}
  |     -> additions, deletions, commit message, files/patches
  |     -> empty-commit detection
  |     -> revert classification (see below)
  |
  +-- GET /repos/{owner}/{repo}/commits/{sha}/pulls
  |     -> associated merged PRs (number, author, head SHA, merge commit SHA, merged_at)
  |
  +-- for each merged PR:
  |     |
  |     +-- GET /repos/{owner}/{repo}/pulls/{number}        (PR detail)
  |     +-- GET /repos/{owner}/{repo}/pulls/{number}/reviews
  |     |     -> reviewer login, state (APPROVED/DISMISSED/CHANGES_REQUESTED/COMMENTED), commit ID, submitted_at
  |     |     -> only reviews on the PR's head SHA count (stale-approval protection)
  |     |     -> only reviews submitted before pr.merged_at count (point-in-time)
  |     |     -> post-merge CHANGES_REQUESTED / DISMISSED -> HasPostMergeConcern
  |     |
  |     +-- GET /repos/{owner}/{repo}/commits/{head_sha}/check-runs
  |     |     -> check name, conclusion (success/failure/...)
  |     |     -> matched against configured required checks
  |     |
  |     +-- GET /repos/{owner}/{repo}/pulls/{number}/commits
  |           -> PR-branch commit authors (for exempt-author short-circuit + self-approval detection)
  |
  +-- if commit message classifies as a manual revert:
  |     +-- GET /repos/{owner}/{repo}/commits/{sha}              (revert's own files)
  |     +-- GET /repos/{owner}/{repo}/commits/{reverted_sha}     (reverted commit's files)
  |           -> compare patches file-by-file; set RevertVerification (diff-verified / diff-mismatch)
  |
  +-- compliance decision:
        is_exempt?  -> COMPLIANT (short-circuit) if all PR-branch authors are exempt too
        is_empty?   -> COMPLIANT (short-circuit) -- 0 additions, 0 deletions
        has_pr?                       -> no: FAIL
        has_approval_on_final_commit? -> no: FAIL (stale or absent approval)
        required_checks_passed?       -> no: FAIL
        yes to all                    -> COMPLIANT
```

Every REST endpoint is fully paginated. No data is silently truncated -- if a pagination boundary is hit, all pages are fetched.

### Per-reviewer state tracking

For SOX compliance, a DISMISSED review overrides an earlier APPROVED review from the same reviewer. gh-audit tracks the latest review state per reviewer on the PR's head commit:

```
reviewer A: APPROVED (10:00)  -> DISMISSED (11:00)  -> final state: DISMISSED
reviewer B: COMMENTED (09:00) -> APPROVED (10:30)   -> final state: APPROVED
```

### Self-approval detection

A review is considered self-approval if the reviewer matches any of:
- PR author
- Commit author
- Committer (excluding GitHub merge bots: "web-flow", "github")
- Co-authors (from `Co-authored-by` trailers)
- Any PR-branch commit author with a non-empty contribution (covers squash-merges where the reviewer's code landed in the squash)

A reviewer whose only PR-branch contribution is a zero-diff "rerun CI" commit is **not** treated as a code author — diff stats are lazy-fetched (DB-cached) to distinguish truly empty admin commits from `/pulls/{n}/commits`'s missing-stats default. Fetch errors fail open (treat as contributor).

If the only approvals are self-approvals, the commit is non-compliant.

### Revert classification

A commit is classified into one of five revert categories from its message, and (for manual reverts) verified by comparing diffs:

| Message pattern | Category | Verification | `IsCleanRevert` |
|---|---|---|---|
| Anything else | `none` | — | false |
| `Automatic revert of <new>..<old>` (bot-generated) | auto-revert | `message-only` — trusted by construction | **true** |
| `Revert "…"` + `This reverts commit <sha>` body, diffs are exact inverses | manual revert | `diff-verified` | **true** |
| `Revert "…"` + referenced commit exists, diffs do **not** match inversely | manual revert | `diff-mismatch` (intervening edits / partial revert / conflict resolution) | false |
| `Revert "…"` + referenced commit can't be fetched or parsed | manual revert | `message-only` (fallback) | false |
| `Revert "Revert "…"` or `Revert "Automatic revert of …"` | revert-of-revert (re-apply) | `none` | false |

The diff-inverse check treats `+` lines and `-` lines as multisets per file. `+++` / `---` file headers and `@@` hunk markers are stripped before comparison. For every filename touched by the revert, the added lines must equal the reverted commit's deleted lines (multiset), and vice versa.

**Why not verify bot reverts?** An auto-revert bot emits clean reverts by construction — its whole job is to produce a pure inverse. Skipping diff verification for auto-reverts keeps the API cost at zero for the common case; manual reverts still pay 2 extra REST calls for their verification.

**Compliance is not affected by revert status.** Clean reverts still go through the normal PR + approval + required-check evaluation. The `IsCleanRevert` / `RevertVerification` fields exist so reviewers can triage reverts separately from novel code in the `Clean Reverts` XLSX sheet.

## Config file (optional)

For advanced use (multi-token, audit rules, exemptions), create `~/.config/gh-audit/config.yaml`:

```yaml
orgs:
  - name: my-org
    exclude_repos: [deprecated-thing]

tokens:
  - kind: pat
    env: GH_AUDIT_TOKEN
    scopes:
      - org: my-org

audit_rules:
  required_checks:
    - name: "Owner Approval"
      conclusion: success
  # Branches that count as part of the audited history. Reports are scoped
  # to commits on one of these branches, which prevents PR-branch commits
  # persisted during enrichment (for squash-merge contributor attribution)
  # from polluting raw counts after a re-evaluate-commits pass.
  #
  # Supports glob patterns: `*` (any chars), `?` (single char). Matching is
  # case-sensitive — list both casings if you need them. Default when unset:
  # ["master", "main"].
  audit_branches:
    - master
    - main
    - "release/*"
    - "HF_BF_*"
    - "hf_bf_*"

sync:
  concurrency: 10
  enrich_concurrency: 16
  initial_lookback_days: 90
  org_repos_cache_freshness: 24h   # default; "0s" disables the cache

exemptions:
  authors:
    # Each entry is a structured map. The matching key is `id` (the
    # immutable GitHub numeric account id); `login` is display
    # metadata. Bare-string entries are no longer accepted — see the
    # 2026-05-04 schema migration in Architecture.md §1 for why.
    - login: "dependabot[bot]"
      id: 49699333
      type: Bot
    - login: "renovate[bot]"
      id: 2740337
      type: Bot
    - login: "li-auto-merge[bot]"
      id: 127378383
      type: Bot
    # Service accounts. id is canonical; login can be any GitHub
    # username and is treated as display only. Comments are
    # preserved through round-trip and are useful for "was: <old>,
    # renamed YYYY-MM" notes.
    - login: svc-tg_LinkedIn
      id: 12345678  # replace with the real numeric id
      type: User
      name: Trunk-Guardian
      comment: was svc-tg, renamed when account migrated to _LinkedIn form
```

### `sync.org_repos_cache_freshness`

Caps how long a cached `/orgs/{org}/repos` enumeration is trusted before re-fetching. Default `24h`; the in-pipeline `ListOrgRepos` short-circuits to a DuckDB-backed cache when the cached listing is fresher than this window, skipping the 60-90s parallel-paginated enumeration on every subsequent run.

Override at the command line with `gh-audit sync --org-repos-cache=<duration>` (e.g. `--org-repos-cache=1h` for tighter freshness, `--org-repos-cache=0s` to disable the cache and always live-fetch). The flag overrides the config file value.

When you add or remove a repo in the org and want gh-audit to see it immediately, pass `--org-repos-cache=0s` for that one run.

The exempt shortcut only fires when every PR-branch commit author is also in this list. See [Exempt authors](#exempt-authors).

## Re-evaluate commits

`gh-audit re-evaluate-commits` re-runs the compliance evaluation on every commit in the database without making any GitHub API calls. It uses the enrichment data (PRs, reviews, check runs) already stored from a previous sync, and runs in a single pass — every revert is judged standalone.

Use this after changing audit rules (e.g. adding a required check) or exempt authors in your config — no need to re-fetch everything from GitHub.

The legacy command name `re-audit` continues to work as an alias.

## Config management

```bash
# Check that your config file is valid YAML with correct structure
gh-audit config validate

# Print the fully resolved config (useful for debugging token scoping)
gh-audit config show
```

## Reports

```bash
# Terminal table
gh-audit report

# Only failures
gh-audit report --only-failures

# XLSX for auditors
gh-audit report --format xlsx --output compliance-q1-2026.xlsx

# JSON / CSV
gh-audit report --format json
gh-audit report --format csv --output audit.csv

# Filter by org, repo, or date range
gh-audit report --repo my-org/my-repo --since 2026-01-01 --until 2026-04-01
```

### XLSX output

The `--format xlsx` output produces a workbook with 9 sheets:

| Sheet | Purpose |
|---|---|
| **Summary** | Rollup by org/repo -- compliance counts and percentages |
| **All Commits** | Every commit with hyperlinked SHA and PR # |
| **Non-Compliant** | Failures only, with empty "Resolution" column for auditor notes |
| **Exemptions** | Bot and empty commits with exemption reasons |
| **Self-Approved** | Commits where the only approval came from a code contributor |
| **Stale Approvals** | Commits merged after approval became stale (force-push after review) |
| **Post-Merge Concerns** | PRs where a reviewer flipped to CHANGES_REQUESTED / DISMISSED after merge |
| **Clean Reverts** | Bot auto-reverts and diff-verified manual reverts — triage separately from novel code |
| **Multiple PRs** | Commits associated with more than one PR (one row per commit-PR pair) |

## Architecture

- **Go CLI** with [cobra](https://github.com/spf13/cobra)
- **DuckDB** for local storage and analytics
- **GitHub REST API** for all data fetching (commits, PRs, reviews, check runs)
- **Token pool** with rate-limit-aware selection across multiple PATs and App tokens

## Authentication

gh-audit supports three token sources. You can mix PAT and App tokens in the same pool.

### Auto-detected token (zero config)

With no config file, gh-audit tries in order: `GH_TOKEN` env var, `GITHUB_TOKEN` env var, then `gh auth token` (GitHub CLI). The first one found is used as a wildcard token for all orgs/repos.

### Personal Access Tokens (PATs)

```yaml
tokens:
  - kind: pat
    env: GH_AUDIT_TOKEN          # env var holding the token
    scopes:
      - org: my-org              # restrict to this org
      - org: other-org
        repos: [repo-a, repo-b]  # restrict to specific repos
```

### GitHub App installation tokens

```yaml
tokens:
  - kind: app
    app_id: 12345
    installation_id: 67890
    private_key_path: /path/to/app.pem   # or private_key_env: APP_KEY
    scopes:
      - org: my-org
```

App tokens are generated at runtime via JWT exchange using the [ghinstallation](https://github.com/bradleyfalzon/ghinstallation) library. They auto-refresh before expiry.

### Required GitHub permissions

gh-audit calls read-only REST endpoints. The minimum permissions needed:

| Permission | Scope | Why |
|---|---|---|
| **Contents** | Read | `GET /repos/{o}/{r}/commits` — list and fetch commit details |
| **Pull requests** | Read | `GET /repos/{o}/{r}/commits/{sha}/pulls` — find associated PRs; `GET /repos/{o}/{r}/pulls/{n}/reviews` — fetch review states |
| **Checks** | Read | `GET /repos/{o}/{r}/commits/{ref}/check-runs` — verify required status checks |
| **Metadata** | Read | `GET /repos/{o}/{r}` and `GET /orgs/{org}/repos` — repo discovery and default branch detection |

For a **classic PAT**, the `repo` scope covers all of the above.

For a **fine-grained PAT**, enable: Contents (read), Pull requests (read), Checks (read), and Metadata (read).

For a **GitHub App**, set these repository permissions during app creation: Contents (read), Pull requests (read), Checks (read), and Metadata (read). Install the app on the target org/repos.

### Token pool and rate limits

Multiple tokens (PAT, App, or a mix) can be configured. The token pool:

- Tracks `x-ratelimit-remaining` and `x-ratelimit-reset` from every response
- Routes each request to the token with the most remaining quota that matches the target org/repo scope
- Blocks and waits for reset when all matching tokens are exhausted
- Retries on 429 (respecting `Retry-After` header)
- Disables tokens permanently on 401 (invalid credentials)

GitHub REST API: 5,000 requests/hour per PAT (higher for GitHub Apps). Typical cost per commit: ~6 requests (commit detail + PRs list + PR detail + reviews + check runs + PR commits). Commits that are manual reverts add 2 more requests for diff-inverse verification. One token audits ~800-1,000 commits/hour. Multiple tokens multiply throughput linearly.

The pipeline emits periodic telemetry every 10 seconds showing commits/sec rates and per-pool API budget headroom:

```
level=INFO msg=telemetry elapsed=30s commits_synced=244 commits_audited=54 \
  sync_rate_recent=8.1/s audit_rate_recent=1.8/s sync_rate_total=8.1/s \
  tokens_available=6/6 api_budget_used_pct=53.0% api_budget_remaining=32896 api_budget_capacity=60000
```

## Global flags

```
--config    Config file path (default: ~/.config/gh-audit/config.yaml)
--db        DuckDB database path (default: ~/.local/share/gh-audit/audit.db)
--verbose   Enable debug logging
```
