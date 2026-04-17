# gh-audit

Enterprise-scale GitHub commit compliance auditor. Verifies that every commit on protected branches was properly code-reviewed, approved on its final commit, and passed required status checks. Built for SOX/SOC2 audit evidence.

## What it checks

For every commit on configured branches:

1. **Has an associated merged PR** — direct pushes are flagged
2. **Approved on the final commit** — stale approvals (before force-push) don't count
3. **Not self-approved** — reviewer must not be the PR author, commit author, committer, or co-author
4. **Required checks passed** — e.g. "Owner Approval" check ran successfully on the PR's head commit
5. **Bot exemptions** — configurable list of bot accounts that skip review requirements
6. **Empty commits** — flagged but not counted as violations

## Install

```
go install github.com/stefanpenner/gh-audit@latest
```

Or build from source:

```
git clone https://github.com/stefanpenner/gh-audit.git
cd gh-audit
go build -o gh-audit .
```

## Quick start

### 1. Create a config file

```yaml
# ~/.config/gh-audit/config.yaml

orgs:
  - name: my-org
    branches: [main, master, "release/*"]
    exclude_repos: [my-org/deprecated-thing]

tokens:
  - kind: pat
    env: GITHUB_TOKEN
    scopes:
      - org: my-org

audit_rules:
  require_pr: true
  require_approval: true
  required_checks:
    - name: "Owner Approval"
      conclusion: success

sync:
  concurrency: 10
  initial_lookback_days: 90

exemptions:
  authors:
    - "dependabot[bot]"
    - "renovate[bot]"
```

### 2. Sync data from GitHub

```bash
export GITHUB_TOKEN=ghp_...

# Sync all configured orgs
gh-audit sync

# Sync a specific repo
gh-audit sync --repo my-org/my-repo

# Sync a specific date range
gh-audit sync --since 2026-01-01 --until 2026-04-01

# Higher concurrency for large orgs
gh-audit sync --concurrency 20
```

### 3. Generate reports

```bash
# Terminal table
gh-audit report

# Only show failures
gh-audit report --only-failures

# XLSX for auditors (5 sheets: Summary, All Commits, Non-Compliant, Exemptions, Self-Approved)
gh-audit report --format xlsx --output compliance-q1-2026.xlsx

# CSV for downstream processing
gh-audit report --format csv --output audit.csv

# JSON
gh-audit report --format json

# Filter by org, repo, or date range
gh-audit report --org my-org --since 2026-01-01 --until 2026-04-01
```

### 4. Validate config

```bash
gh-audit config validate
gh-audit config show
```

## Token configuration

### Personal Access Tokens (PAT)

```yaml
tokens:
  - kind: pat
    env: GH_AUDIT_PAT_1     # reads token from this env var
    scopes:
      - org: my-org
      - org: other-org
```

### GitHub App installation tokens

```yaml
tokens:
  - kind: app
    app_id: 12345
    installation_id: 67890
    private_key_path: /path/to/app.pem
    scopes:
      - org: my-org
```

Multiple tokens are load-balanced automatically. The token pool tracks rate limits per token and picks the one with the most remaining quota.

## Multi-branch auditing

By default, only each repo's default branch is audited. To audit release branches:

```yaml
orgs:
  - name: my-org
    branches:
      - main
      - master
      - "release/*"
      - "v*"
```

Each branch gets its own sync cursor, so incremental syncs are efficient.

## XLSX report for auditors

The `--format xlsx` output produces a workbook with 5 sheets:

| Sheet | Purpose |
|---|---|
| **Summary** | Rollup by org/repo — total, compliant, non-compliant, self-approved, bots, compliance % |
| **All Commits** | Every commit with hyperlinked SHA and PR #, auto-filters on all columns |
| **Non-Compliant** | Failures only, with an empty "Resolution" column for auditor notes |
| **Exemptions** | Bot and empty commits listed with exemption reasons |
| **Self-Approved** | Commits where the only approval came from a code contributor |

All SHA and PR cells are clickable hyperlinks to GitHub.

## Global flags

```
--config    Config file path (default: ~/.config/gh-audit/config.yaml)
--db        DuckDB database path (default: ~/.local/share/gh-audit/audit.db)
--verbose   Enable debug logging
```

## Architecture

- **Go CLI** with [cobra](https://github.com/spf13/cobra)
- **DuckDB** for local storage and analytics
- **GitHub GraphQL API** for batched enrichment (25 commits per query)
- **GitHub REST API** for commit listing and pagination fallback
- **Token pool** with rate-limit-aware selection across multiple PATs and App tokens

## GitHub's org audit log

GitHub provides an [organization audit log](https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/reviewing-the-audit-log-for-your-organization) that tracks administrative actions (branch protection changes, team membership, deploy keys, etc.). It answers "who changed the rules?" while gh-audit answers "did everyone follow the rules?" They're complementary -- see [docs/github-audit-log-comparison.md](docs/github-audit-log-comparison.md) for the full breakdown.

## Future enhancements

- **Webhook-based sync**: Use GitHub `push` event webhooks for real-time updates after initial historical sync
- **Cherry-pick detection**: Parse `(cherry picked from commit <sha>)` messages to link cherry-picks back to reviewed PRs
- **Audit log integration**: Detect admin branch protection bypasses via `protected_branch.policy_override` events
