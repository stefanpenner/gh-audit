# gh-audit

GitHub commit compliance auditor. (Proof of concept.)

It checks that every commit on a protected branch was:

- reviewed and approved on its final commit,
- approved by someone other than the author, and
- passed the required status checks.

It is built for SOX / SOC2 audit evidence. It is **read-only** — it never
writes to GitHub.

This README is the user manual: how to install, run, and configure the tool.
For how it makes its decisions and why those decisions are trustworthy, see
**[Architecture.md](Architecture.md)**.

## What it checks

For every commit on a configured branch, gh-audit runs these rules in order:

1. **Exempt author** — author is on the exempt list (and any squashed-in code
   is also exempt). Compliant.
2. **Empty commit** — zero lines and zero files changed. Compliant.
3. **Has a merged PR** — no PR means a direct push. Flagged.
4. **Approved on the final commit** — at merge time, on the final SHA.
5. **Not self-approved** — the approver is not the author.
6. **Required checks passed** — e.g. an "Owner Approval" check.
7. **Verdict** — compliant if a PR clears rules 4–6.
8. **Clean-revert waiver** — a verified clean revert is waived to compliant.

This is a summary. The full decision tree, the data each rule trusts, and the
trust model live in [Architecture.md](Architecture.md#what-gh-audit-detects).

## Install

```
go install github.com/stefanpenner/gh-audit@latest
```

## Quick start

No config file needed. If the `gh` CLI is logged in, just run:

```bash
# Audit one repo
gh-audit sync --repo my-org/my-repo

# Audit a whole org
gh-audit sync --org my-org

# Audit a date range
gh-audit sync --repo my-org/my-repo --since 2026-01-01 --until 2026-04-01

# Audit all history (aliases: all, beginning)
gh-audit sync --repo my-org/my-repo --since epoch

# Report the failures
gh-audit report --only-failures
```

The token is auto-detected: `GH_TOKEN`, then `GITHUB_TOKEN`, then
`gh auth token`. See [Authentication](#authentication) to configure your own.

## Commands

| Command | What it does |
|---|---|
| `sync` | Fetch from GitHub, enrich, and audit. The main loop. |
| `report` | Print or export the results. See [Reports](#reports). |
| `re-evaluate-commits` | Re-audit from the local database. No API calls. |
| `config validate` | Check the config file is valid. |
| `config show` | Print the fully resolved config. |
| `backfill-missing-prs` | Recover PR links for "no PR" rows. |
| `annotate-commits` | Recompute message annotations. No API calls. |

### Re-evaluate after a config change

`re-evaluate-commits` re-runs the audit on every commit already in the
database. It uses the stored PRs, reviews, and checks — **no GitHub calls**.

Run it after you change audit rules or exempt authors. You do not need to
re-fetch from GitHub.

The old name `re-audit` still works.

## Reports

```bash
gh-audit report                                   # terminal table
gh-audit report --only-failures                   # failures only
gh-audit report --format xlsx --output q1.xlsx    # XLSX for auditors
gh-audit report --format json                     # JSON
gh-audit report --format csv --output audit.csv   # CSV
gh-audit report --repo my-org/my-repo --since 2026-01-01   # filter
```

### XLSX workbook

`--format xlsx` produces an 8-sheet workbook, ordered Action → Overview →
Trace. (`--only-failures` is not supported for XLSX; filter in Excel instead.)

| Sheet | Purpose |
|---|---|
| **README** | Legend for the rule codes and cell values. |
| **Action Queue** | Non-compliant commits to act on, by severity. |
| **Summary** | Rollup by org / repo, with compliance %. |
| **By Rule** | One row per rule: fires, outcomes, top repo / author. |
| **By Author** | Rollup by author, worst first. |
| **Decision Matrix** | One row per commit, one column per rule. Autofilter it. |
| **Waivers Log** | Each waiver and the evidence behind it. |
| **Multiple PRs** | Commits linked to more than one PR. |

The sheet layout is described in
[Architecture.md](Architecture.md#report-layer).

## Configuration

A config file is optional. Use it for multiple tokens, audit rules, and
exemptions. Default path: `~/.config/gh-audit/config.yaml`.

Rules:

- A missing file at the default path is fine. Built-in defaults apply.
- Pointing `--config` at a missing file is an error.
- An invalid config (bad YAML or failed validation) is always fatal.
  gh-audit never falls back to default rules silently — that could produce
  wrong verdicts.
- A leading `~` is expanded in `database:` and `private_key_path:`.

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
  # Required status checks. Matched against Checks-API runs first; if a name
  # is missing there, the legacy commit-status API (older Jenkins) is checked.
  required_checks:
    - name: "Owner Approval"
      conclusion: success

  # Branches that count as audited history. Reports are scoped to these.
  # Globs allowed: * and ?. Matching is case-sensitive. Default: [master, main].
  audit_branches:
    - master
    - main
    - "release/*"

sync:
  concurrency: 10
  enrich_concurrency: 16
  initial_lookback_days: 90
  org_repos_cache_freshness: 24h   # "0s" disables the cache

exemptions:
  # Matched by immutable numeric account id ONLY. `login` is display only.
  # There is no email path — a git-author email is forgeable, so matching it
  # would let any pusher forge an exemption. Service accounts must have a
  # GitHub account id. (The old `verified_emails` key is now rejected at load
  # time.) See Architecture.md §1.
  authors:
    - login: "dependabot[bot]"
      id: 49699333
      type: Bot
    - login: svc-tg_LinkedIn
      id: 12345678        # the real numeric id
      type: User
      name: Trunk-Guardian
      comment: was svc-tg, renamed on migration
```

### `org_repos_cache_freshness`

Caps how long a cached `/orgs/{org}/repos` listing is trusted. Default `24h`.

A fresh cache skips the 60–90s repo enumeration on every run.

Override per run:

```bash
gh-audit sync --org-repos-cache=1h    # tighter freshness
gh-audit sync --org-repos-cache=0s    # disable; always live-fetch
```

Use `0s` for the run right after you add or remove a repo in the org.

## Authentication

gh-audit takes three token sources. You can mix PAT and App tokens in one pool.

### Auto-detected (zero config)

With no config file, gh-audit tries, in order: `GH_TOKEN`, `GITHUB_TOKEN`,
then `gh auth token`. The first one found covers all orgs and repos.

### Personal Access Token (PAT)

```yaml
tokens:
  - kind: pat
    env: GH_AUDIT_TOKEN          # env var holding the token
    scopes:
      - org: my-org              # restrict to this org
      - org: other-org
        repos: [repo-a, repo-b]  # or to specific repos
```

### GitHub App token

```yaml
tokens:
  - kind: app
    app_id: 12345
    installation_id: 67890
    private_key_path: /path/to/app.pem   # or private_key_env: APP_KEY
    scopes:
      - org: my-org
```

App tokens are minted at runtime by JWT exchange via
[ghinstallation](https://github.com/bradleyfalzon/ghinstallation), and
auto-refresh before expiry.

How the pool routes requests and handles rate limits is covered in
[Architecture.md](Architecture.md#token-pool).

### Required permissions

gh-audit calls only read-only endpoints. The minimum permissions:

| Permission | Scope | Used for |
|---|---|---|
| **Contents** | Read | List and fetch commits. |
| **Pull requests** | Read | Find PRs and fetch review states. |
| **Checks** | Read | Verify required status checks. |
| **Metadata** | Read | Discover repos and default branches. |

- **Classic PAT**: the `repo` scope covers all of these.
- **Fine-grained PAT**: enable the four read permissions above.
- **GitHub App**: set the four as repository permissions, then install the app
  on the target org or repos.

## Global flags

```
--config    Config file path (default: ~/.config/gh-audit/config.yaml)
--db        DuckDB database path (default: ~/.local/share/gh-audit/audit.db)
--verbose   Enable debug logging
```

Notes:

- `--since` / `--until` take ISO 8601 dates. `--until` must not be before
  `--since`.
- `--org` cannot be combined with `--repo` (a repo already names its org).
- SIGINT / SIGTERM cancel cleanly: in-flight DB writes finish or roll back. A
  second signal hard-kills.
