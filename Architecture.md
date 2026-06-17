# Architecture

This is the design reference: how gh-audit decides, and why those decisions
can be trusted. For how to install, run, and configure it, see
[README.md](README.md).

## Trust model

A verdict is only as trustworthy as the data under it. This section is the one
place that traces every input: what we rely on, where it comes from, whether it
is forgeable on its own, and what makes it trustworthy anyway. The per-rule
sections below tag their inputs with `← SOT:` (source of truth); this table
gathers them.

### Root of trust

Three things, and nothing else:

1. **GitHub's identity binding.** A numeric account id (`user.id`) is set by
   GitHub from a verified account. It is immutable, never reused, and not
   forgeable by a client. To attribute a commit or review to another account's
   id, you must compromise that account. All identity matching (§4 approvals,
   §5 self-approval) is **id-only**, via `TrustedID` — non-zero and not the
   shared ghost sentinel `10137`. Never login strings.
2. **GitHub-set canonical fields.** Values GitHub writes itself, that a pushing
   client cannot influence:
   - `pr.merge_commit_sha` (set at merge),
   - content-addressed commit and parent SHAs (forging one changes the
     commit's own SHA),
   - check-run conclusions,
   - the `web-flow` verified signature (only GitHub holds the signing key).
3. **The operator-curated exempt list** (`exemptions.authors`: ids plus
   `verified_emails`). This is a deliberate trust root. The operator vets which
   bot and service accounts may ship without human review.

The GitHub token is **read-only** (see [Required permissions](README.md#required-permissions)).
gh-audit never writes, so it cannot perturb its own evidence.

### What each verdict input rests on

| Signal | Source (endpoint) | Forgeable alone? | What makes it trustworthy | Feeds |
|---|---|:---:|---|---|
| `author_id`, `reviewer.id`, PR/branch author ids | `GET /commits/{sha}`, `/pulls/{n}/reviews`, `/pulls/{n}/commits` → `*.id` | **No** | GitHub binds id to a verified account; `TrustedID` rejects 0/ghost | §1, §4, §5 |
| `review.state`, `commit_id`, `submitted_at` | `/pulls/{n}/reviews` | No | GitHub-recorded; tied to a head SHA the client can't choose | §4 |
| `dismissed_at`, `dismissed_state` | `/issues/{n}/events` | No | GitHub-recorded timeline event | §4 |
| commit **parent SHAs** | commit object | No | Content-addressed — a forged parent changes the commit's own SHA | §4 graph carve-out |
| `pr.merge_commit_sha` | `/pulls/{n}` | No | GitHub sets it atomically at merge time | §3 recovery, §8 manual-revert target |
| check-run conclusions | `/commits/{ref}/check-runs`, `/status` | No | GitHub/CI-reported | §6 |
| `web-flow` committer + `verification.verified` | commit detail | No | Only GitHub holds the web-flow signing key (CleanMerge) | §5 author-skip |
| ManualRevert diff | `GetCommitFiles` | No | Actual patch multiset-compared against the reverted commit | §8 |
| exempt list (id, `verified_emails`) | operator config | n/a (trust root) | Operator-curated and vetted | §1, §4 carve-out |
| `(#N)` squash-message token | commit message | **Yes** | **Verified before trust** — see below | §3 recovery |
| git-author `email` (§1 fallback) | commit detail | **Yes** | Backstopped by a PR + every-contributor check — see below | §1 |
| AutoRevert message prefix | commit message | **Yes** | **Not backstopped** — see below | §8 |

### Leaps through forgeable nodes

A "leap" is trusting a forgeable signal to reach a verdict without first
anchoring it to something non-forgeable. The design is to verify a forgeable
hint against a canonical fact before trusting it. Three inputs are forgeable.
Here is how each is handled.

- **§3 squash `(#N)` token — verified, no leap.** The `(#N)` in a message is a
  forgeable hint. It is accepted only when `pr.merged && pr.merge_commit_sha ==
  sha` (`recoverPRFromMergeMessage`). A message claiming `(#1234)` cannot make
  `pulls/1234.merge_commit_sha` equal this commit's SHA. Forgeable hint →
  non-forgeable check.

- **§1 `verified_emails` fallback — backstopped, no leap.** When `author_id` is
  0, §1 may match the forgeable git-author email against the operator's curated
  list. It is trusted only with an **associated PR**, so the every-contributor
  backstop applies (`hasNonExemptPRContributors`: each PR-branch commit must
  pass the same id-or-email check). A direct push has no PR and no backstop, so
  the email path **fails closed** there (`applyExemptAuthorRule`). The id path
  is unaffected and needs no PR. The residual trust is the operator's curation
  of the list — a declared trust root, not a forged input.

- **§8 AutoRevert — message-only, an accepted leap (documented).** A commit
  whose message matches `^Automatic revert of <sha>..<sha>` is waived compliant
  with **no** identity, signature, or diff check (`caching.go`,
  `RevertVerification = "message-only"`). The message is forgeable, so this is
  a leap through a forgeable node. It is kept by operator decision, on the
  rationale "trust bot-generated auto-reverts" — accepting that nothing proves
  the bot authored it. Contrast `ManualRevert`, which is diff-verified. Tracked
  in `TODO.md` ("AutoRevert waiver rests on a forgeable commit message") with
  two closure options: gate on a trusted author id, or require diff
  verification.

Everything else that could be forged — committer login, `Co-authored-by`
trailers — is **excluded from compliance entirely** (see §5, "Excluded identity
sources"). The §4 graph carve-out replaced a forgeable committer timestamp with
positional graph ancestry. The timestamp path survives only as a transitional
fallback for rows synced before parent SHAs were persisted; one online re-sync
upgrades them.

## What gh-audit detects

For every commit on a protected branch, gh-audit runs a decision tree in order.

- Rules 1–6 are the primary check.
- Rules 7–8 are fallbacks that can still flip a non-compliant verdict to
  compliant.
- A separate `HasPostMergeConcern` flag is orthogonal to compliance. It tracks
  reviews submitted **after** merge (see rule 4).

### 1. Exempt author

If the commit author is on the exempt list, the commit is **compliant** at
once. No further rules run.

The match prefers the **numeric account id** (`commit.author_id` vs
`ExemptAuthor.id`). GitHub sets this id from a verified account. It is
immutable, never reused, and not forgeable by a client.

**Squash backstop.** A bot can squash human code into one commit. So the
exemption holds only when every PR-branch commit is also exempt
(`hasNonExemptPRContributors`). One non-exempt contributor voids it:
`IsExemptAuthor` is still set for visibility, but the squash content is audited
normally.

A non-exempt branch commit is ignored only when it is **verifiably empty** —
zero lines and zero files. `/pulls/{n}/commits` omits diff stats, so emptiness
is confirmed with a lazy `GetCommitDetail` (`StatsTriggerExemption`). The result
is persisted with a `detail_fetched_at` marker (`MarkCommitDetail`), so an
offline re-audit reaches the same verdict. If emptiness can't be verified — no
marker, no fetcher, or a fetch error — it **fails closed** and voids the
carve-out. Trusting locally-zero stats would skip every branch commit and waive
unreviewed code.

**Email fallback.** Some service accounts push with an email GitHub doesn't
bind to an account, so `author_id` arrives as 0. The rule then matches
`commit.author_email` against the entry's curated `verified_emails` list. This
email is forgeable on its own, so it is honored only **when the commit has a
PR** (so the squash backstop applies). A direct push matched only by email
**fails closed** and drops through to §3. The id path is unaffected — a trusted
id is exempt even on a direct push. See the [Trust model](#trust-model).

```
config.yaml: exemptions.authors[]    ← SOT: operator-curated list (id, verified_emails)
      │
      ▼
GET /repos/{o}/{r}/commits/{sha}
      → commit.author_id              ← preferred, set by GitHub from verified email binding
      → commit.author_email           ← fallback when author_id == 0
      │
      ▼
EvaluateCommit (audit.go)
      isExemptCommit(id, email):
        author_id matches exempt.id?         → exempt (even on a direct push)
        author_id == 0 AND email ∈ verified_emails? → exempt ONLY if a PR
                                                exists (subject to the
                                                PR-branch check below);
                                                direct push → fail closed
        else                                 → not exempt, continue to rule 2
      │
      ▼ (when exempt and PR exists)
      hasNonExemptPRContributors():
        any branch commit fails isExemptCommit? → grant IsExemptAuthor flag for visibility,
                                                  but audit the squash content normally
        all branch commits exempt?              → IsCompliant=true, reason="exempt: configured author"
```

### 2. Empty commit

A commit that changes nothing is **compliant** (flagged for visibility, no
review needed). "Nothing" means all three are zero: added lines, deleted lines,
and files touched.

The file count matters. GitHub reports `0/0` lines for pure renames and
mode-only changes. A commit that swaps `auth_enabled.go` for `auth_disabled.go`
is not a no-op.

`applyEmptyCommitFallback` (`audit.go`) runs lazily, only on paths heading to
non-compliant: once when there is no PR, and again after all PRs fail. Compliant
commits skip the `GetCommitDetail` call entirely.

A stats-fetch **error fails closed**. The waiver does not fire and the commit
stays non-compliant (re-audit can recover it). Treating an unresolved zero as
"empty" would turn a transient API blip into a permanent compliant row.

Offline re-audit (nil fetcher) keeps the old "stored zero stats → empty" reading
for rows never detail-fetched. Rows verified at sync time carry their file count
(`files_changed` + `detail_fetched_at`), so verified rename-only commits stay
blocked offline too.

```
GET /repos/{o}/{r}/commits/{sha}
      → commit.additions, commit.deletions   ← SOT: GitHub REST API (commit detail)
      │
      ▼
EvaluateCommit (audit.go)
      → additions == 0 && deletions == 0 && files_changed == 0?
          yes → IsCompliant=true, IsEmptyCommit=true, reason="empty commit"
          no  → continue to rule 3
```

### 3. Has associated PR

If the commit has no merged PR (a direct push), it is **non-compliant**. Reason:
`no associated pull request`.

```
GET /repos/{o}/{r}/commits/{sha}/pulls
      → []PullRequest (merged only)           ← SOT: GitHub REST API (best-effort index)
      │
      ▼
EvaluateCommit (audit.go)
      → len(PRs) == 0?
          yes → recover via parse + canonical verify (see below)
                → still 0? IsCompliant=false, HasPR=false, reason="no associated pull request"
          no  → PRCount=len(PRs), continue to rule 4
```

#### Commit→PR index gap

GitHub exposes the commit↔PR link through two surfaces:

| Direction | Source | Trustworthiness |
|---|---|---|
| Commit → PR | `GET /commits/{sha}/pulls` (and GraphQL `Commit.associatedPullRequests`, REST/GraphQL search by SHA) | **Best-effort reverse index.** Asynchronous, computed by GitHub from refs and ref-events. Has observed gaps from indexer races on burst merges, schema migrations, and squash/rebase SHA chases. No SLA. |
| PR → commit | `PullRequest.merge_commit_sha` | **Canonical.** Set by GitHub atomically at merge time. Immutable. Never the gap. |

In the last full sweep (242 repos × 30 days), about 0.12% of commits surfaced
as "no associated pull request" although the PR clearly existed:
`/commits/{sha}/pulls` returned `[]` while `/pulls/{N}.merge_commit_sha` matched
the audited SHA. No alternative discovery API (GraphQL or REST search) recovered
the link either.

##### Mitigation: parse + canonical verify

When `/commits/{sha}/pulls` returns empty, the caching layer
(`recoverPRFromMergeMessage` in `internal/github/caching.go`) tries to recover
the link:

1. **Parse** the trailing `(#N)` token from the squash-merge commit's first line
   (`ParsePRReference`, `internal/github/merge.go`). The regex `\(#(\d+)\)\s*$`
   anchors to the first line, so a revert-of-squash title like
   `Revert "Foo (#100)" (#101)` resolves to `101`, not `100`.
2. **Fetch** PR #N via `getPR` (DB-frozen for previously-synced merged PRs; one
   extra `GET /pulls/N` if cold).
3. **Verify canonically.** Accept the link only if `pr.merged &&
   pr.merge_commit_sha == sha`.

The split is deliberate. The parse step is a forgeable hint — an author can
write any `(#N)` into the message. The verify step is not forgeable: only GitHub
sets `merge_commit_sha`, and only on a real merge of that PR.

§3 still fires in these cases:

- The message has no `(#N)` (cross-fork PRs without an annotation, local manual
  merges).
- The parsed PR exists but isn't merged, or its `merge_commit_sha` doesn't match
  the audited SHA — verification rejects the hint.
- Fetch error — fail closed; never accept an unverified link.

Telemetry reports recovery counts as `pr_recovered` in the per-endpoint
breakdown, so we can track how often the gap fires in production.

### 4. Approval on final commit

For each merged PR, gh-audit builds a per-reviewer state map on the PR's head
SHA. Only reviews on the final commit count.

**Per-reviewer resolution.** If a reviewer submits several reviews on the final
commit, only the latest state-changing one wins:

- A `DISMISSED` or `CHANGES_REQUESTED` at 11:00 overrides an `APPROVED` at 10:00.
- A later plain `COMMENTED` does **not** clobber an earlier `APPROVED` from the
  same reviewer. This matches GitHub's UI: commenting after approving leaves the
  approval intact.

**Post-merge cutoff.** Reviews submitted after `pr.merged_at` are excluded from
compliance. A post-merge `DISMISSED` or `CHANGES_REQUESTED` instead sets
`HasPostMergeConcern=true`, so auditors see the concern without the commit
flipping state.

**Dismissal resolution.** GitHub dismisses a review by mutating it in place:
`state` flips to `DISMISSED`, but `submitted_at` and `commit_id` keep their
original values. The dismissal time and the state at that moment live only in
issue-events (`review_dismissed`). When a fetched PR carries a `DISMISSED`
review, the enricher resolves it (one extra `GET /issues/{n}/events`, only for
PRs with dismissals) and persists `reviews.dismissed_at` / `dismissed_state`. §4
then rules exactly:

- dismissal **after** merge → the review held its original state at merge time.
  An `approved` original is restored for the point-in-time fold (the commit
  stays compliant), and the dismissal sets `HasPostMergeConcern`.
- dismissal **before** merge → an unambiguous non-approval. Nothing flagged.
- dismissal time **unknown** (rows synced before this feature) → fail closed
  (never an approval), and set `HasPostMergeConcern` so an auditor decides.

**Untrusted identities.** A review or PR attributed to an unresolved account
(`id == 0`) or to GitHub's ghost user (`id == 10137`, used for every deleted
account) is never trusted. It cannot count as an independent approval, nor prove
self-approval — two different deleted people both surface as ghost.

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

**Stale approval.** When there is no approval on the final commit but one exists
on an earlier SHA, the reason is `approval is stale — not on final commit`
rather than `no approval on final commit`. This separates "never reviewed" from
"reviewed, then code changed."

#### Exempt-author post-approval carve-out

Many orgs run CI that auto-merges the base branch (e.g. `master`) into open PR
branches to keep them current, or applies routine post-approval automation
(dependency bumps, autoformatting, sync merges). Each such commit moves the PR's
head SHA without adding human code that needs review. Naïvely, that fires §4
stale-approval against any PR whose reviewer approved before the bot ran.

The carve-out (`isApprovalRefreshable` in `internal/sync/audit.go`) promotes
such an approval to `approvalOnFinal` when **every** PR-branch commit after the
approval passes the **same `isExemptCommit` check §1 uses** (numeric id, with
the curated `verified_emails` fallback when the id is unresolved). The PR's own
`merge_commit_sha` is skipped first: `commit_prs ⨝ commits` pulls the
squash-merge commit on master into the per-PR list, and a human-authored squash
commit (the normal case) would otherwise always void the carve-out.

The exempt-author id is the trust boundary, and it is not forgeable. GitHub
binds `AuthorID` to a verified account. The exempt list is the curated set of
bot/service-account ids the operator already vetted as not needing review (the
same list that drives §1). A local actor can't make a commit look like another
account's id without compromising that account. If §1 trusts these accounts to
ship without review, §4 trusts their post-approval commits not to invalidate the
reviewer's coverage.

**Positional, not temporal.** "After the approved snapshot" is the first-parent
walk from the PR's head down to the approval's `commit_id` (parent SHAs are
persisted at ingestion). Graph ancestry can't be forged by backdating
`GIT_COMMITTER_DATE` — a commit between the approved SHA and the head is on that
walk no matter what its timestamps claim. The walk fails closed: an unreachable
approved SHA (force-push) means no promotion. Rows synced before parent
persistence fall back to the committer-timestamp comparison until one online
re-sync upgrades them.

If any post-approval commit is by a non-exempt account, the original §4
stale-approval verdict stands. The carve-out never weakens compliance when real
human code shipped after the approval.

### 5. No self-approval

A review is self-approval when the reviewer's **immutable numeric GitHub id**
matches any of:

- the PR author (`AuthorID`),
- the commit author (`AuthorID`) — skipped for `CleanMerge` commits (below),
- any **PR-branch commit author** (`AuthorID`) with a non-empty contribution.
  This catches squash-merges where the reviewer's own code landed in the squash.
  Authors whose every PR-branch commit is zero-diff (the classic "Empty commit
  to rerun check") are dropped from this set (see "Empty-commit exclusion").

**ID-only matching.** All identity comparison uses immutable numeric ids, never
logins. Ids are never reused, never moved by renames, and not forgeable. A
review with `ReviewerID == 0` (deleted/ghost, unresolved) is not trusted: it is
neither a self-approval nor an independent approval. This kills login-rename
attacks and casing ambiguity.

**CleanMerge exclusion.** A `CleanMerge` is 2 parents + `Merge pull request #…`
message + `web-flow` committer + verified GitHub signature (see
[ClassifyMerge](#classifymerge-internalgithubmergego)). It cannot contain
author-written code: GitHub's merge button refuses to make one under conflicts,
and the verified `web-flow` signature can't be forged locally. For these commits
the author is just "who clicked merge," so skipping the `AuthorID` check avoids
false positives. `DirtyMerge` (a 2-parent merge missing any signal) and
`OctopusMerge` (3+ parents) may carry author edits, so the check still runs.

**Empty-commit exclusion** (PR-branch authors only). A reviewer who pushed only
zero-diff commits — typically `Empty commit to rerun check` to re-trigger CI —
has not contributed code and must not invalidate their own review. Emptiness is
checked against the commit's actual `additions`/`deletions`. The
`/pulls/{n}/commits` listing omits diff stats, so when an author's contributions
all look zero locally, `GetCommitDetail` is fetched lazily (DB-cached) to tell a
truly empty commit from un-fetched stats. Any non-zero stat short-circuits
before any API call. A fetch error fails open (treat as contributor), so we
never silently downgrade a real contributor.

**Excluded identity sources** (intentionally not checked):

- **Committer login** — GitHub provides no committer id on the commit object.
  Login-only comparison is mutable and forgery-prone.
- **Co-authored-by trailers** — unvalidated message text, trivially forgeable.
  No API-resolved id.

If the only approvals are self-approvals (or all from unresolved identities),
the commit is **non-compliant**.

```
review.ReviewerID                    ← SOT: GitHub REST API (reviews → user.id)
      │
      ▼
isSelfApproval (audit.go) — ID-only matching via sameUser():
      │
      ├── pr.AuthorID                    ← SOT: GET /commits/{sha}/pulls → user.id
      ├── commit.AuthorID                ← SOT: GET /commits/{sha} → author.id (skip if CleanMerge)
      └── pr_branch_commits[].AuthorID   ← SOT: GET /pulls/{n}/commits → author.id
                                              (filtered: drop authors whose every contribution is empty;
                                              GetCommitDetail fetched lazily when local stats are zero)
      │
      ▼
All approvals are self (or ReviewerID==0)?
      yes → IsSelfApproved=true, reason="self-approved (reviewer is code author)"
      no  → at least one verified independent approval exists, continue to rule 6
```

### 6. Required status checks

Configured checks (e.g. `Owner Approval`) must appear on the PR's head SHA with
the expected conclusion. A missing or failed check makes the commit
**non-compliant**. A check whose only runs are still queued or in-progress
reports `missing` — it has not failed, it just hasn't concluded.

**Legacy status contexts.** `required_checks` names are matched against
Checks-API runs first. If a name is absent there, the enricher also fetches the
combined commit status (`GET /commits/{ref}/status`) and merges each context in
as a synthetic check run:

- success / failure / error → completed with that conclusion;
- pending → in-progress;
- ids are negated, so the two id spaces can't collide in `check_runs`.

So CI that reports through the legacy `/statuses` API (older Jenkins) satisfies
§6 like any other check — at zero extra cost for all-Checks-API repos.

The same check name can appear many times on one SHA (re-runs mint new ids; the
DB accumulates them across syncs). Only the **latest** run per name counts —
selected by `completed_at`, with `check_run_id` as tiebreak. This mirrors
GitHub's "latest run wins" UI.

```
config.yaml: audit_rules.required_checks   ← SOT: user-configured list
      │
      ▼
GET /repos/{o}/{r}/commits/{head_sha}/check-runs
      → []CheckRun (check_name, conclusion)   ← SOT: GitHub REST API
      │
      ▼
evaluateRequiredChecks (audit.go)
      → for each required check (latest same-named run only):
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

A commit is **compliant** when at least one associated PR has both:

- a non-self approval on the final commit, and
- all required checks passed.

If a commit has several PRs, gh-audit reports the one closest to compliant. The
total PR count is recorded (`pr_count`); commits with `pr_count > 1` appear in
the "Multiple PRs" report sheet.

### 8. Clean-revert waiver (standalone)

If the verdict so far is **non-compliant**, one last check runs. It runs even on
the §3 no-PR path, so a diff-verified clean revert pushed straight to the branch
is waived too. It is per-commit: it does not look at the reverted commit's own
verdict (see `TODO.md` for the deferred cross-commit variant).

A `IsCleanRevert=true` commit is **compliant**. That signal means one of:

- `AutoRevert` — bot-generated, trusted by construction (message-only; see the
  [Trust model](#trust-model) for the forgeability caveat).
- `ManualRevert` whose diff was verified as the exact inverse of the reverted
  commit (`revert_verification = "diff-verified"`).

Every other revert shape — conflict-resolved (`diff-mismatch`), message-only,
revert-of-revert, hand-crafted — falls through to the normal PR-approval rules.
Provenance alone (`committer == web-flow`, a verified signature) is **not**
enough: if the diff isn't a pure inverse, there are new bytes on master, and
those bytes deserve review.

```
non-compliant verdict from rules 1–7 (incl. "no associated pull request")
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

The pipeline runs per-repo, per-branch. Repos sync in parallel (bounded by
`concurrency`). Each branch runs these phases.

### Phase 1: Fetch commits

```
fetchBranchCommits (pipeline.go)
  ├── graph path:    GetBranchHead + CompareCommits(last_sha...head)
  └── fallback:      ListCommits(org, repo, branch, since, until)
  │
  ▼
UpsertCommits ──▶ commits table
UpsertCommitBranches ──▶ commit_branches table
```

**Graph path (preferred).** The cursor stores the branch tip SHA seen at the end
of the last sync (`sync_cursors.last_sha`). An incremental sync (no explicit
`--since`/`--until`) fetches the current tip (`GET /branches/{branch}`):

- tip unchanged → zero new commits, one API call (the unaudited mop-up still
  runs);
- tip moved → `GET /compare/{last_sha}...{head}` returns exactly the commits
  reachable from the new tip but not the old one.

The compare is **graph-based**. So commits pushed with a backdated
`GIT_COMMITTER_DATE` — invisible to the date-filtered list endpoint — are still
ingested. This closes the evasion hole where an attacker hides a direct push by
backdating the committer timestamp.

**Date-window fallback.** Used for explicit `--since`/`--until` runs, legacy
cursors without a SHA, first-time syncs, and when compare can't serve the range
(base force-pushed away → 404, or the range exceeds the compare API's 250-commit
ceiling). The `since` date comes from, in priority order:

1. The `--since` CLI flag. An ISO 8601 date, or `epoch`/`all`/`beginning` for
   full history (these map to a 1970-01-01 sentinel that predates GitHub, so the
   API returns every commit).
2. The stored cursor date for this org/repo/branch, **minus a 72h overlap**.
   This catches honest stale pushes with older committer dates; upserts are
   idempotent and already-audited commits skip enrichment.
3. The `initial_lookback_days` config (default 90).

After either path, the cursor records the new tip SHA and the newest committer
date seen. On the fallback path the tip is the first listed commit — the list
endpoint returns newest-first from the ref tip. The date watermark never
regresses.

A zero-commit fetch window does **not** end the branch sync. The unaudited
mop-up below still runs, so backlog from a prior failed run is cleared even on
dormant branches. Within one repo, branches fetch in parallel, but the
enrich+audit phase is serialized — the unaudited set is repo-scoped, and
parallel branches would duplicate the same work.

**`commit_branches` column provenance:**

| Column | Source |
|--------|--------|
| `org` | YAML config — the organisation key under `orgs:` |
| `repo` | YAML config repos list, or auto-discovered via `GET /orgs/{org}/repos` |
| `sha` | Each commit's SHA returned by `GET /repos/{o}/{r}/commits?sha={branch}&since=…&until=…` (fully paginated) |
| `branch` | YAML config `branches:` list for the org; falls back to the repo's default branch if unset |

### Phase 2: Enrich

For each unaudited commit (no row in `audit_results`), the enricher draws on six
REST endpoints. The first is DB-first and usually skipped (see [Caching
layer](#caching-layer)).

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
  ├──▶ GET /repos/{o}/{r}/pulls/{n}/reviews     (per PR)
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

Enrichment runs in parallel batches: 25 commits per batch, bounded by
`enrich_concurrency`. Inside a batch, commits run concurrently (bounded by
`enrichCommitFanout`, default 10), and PRs within one commit run concurrently
too (bounded by `enrichPRFanout`, default 5). All endpoints are fully paginated
— no silent truncation.

Enrichment goes through `CachingEnricher` (see [Caching layer](#caching-layer)),
which resolves many calls from the DB instead of the API and tracks per-endpoint
hit/miss counts in `APIStats`.

Results are deduplicated by primary key before writing:

```
UpsertReviews ──▶ reviews table              (PENDING drafts filtered out)
UpsertCheckRuns ──▶ check_runs table
InsertCommitsIfAbsent ──▶ commits table      (PR-branch commits; never clobbers rich rows)
UpsertCommitPRs ──▶ commit_prs table
UpsertPullRequests ──▶ pull_requests table   (LAST — see below)
```

Two ordering rules are load-bearing:

- **PR rows are written last.** The merged-PR freeze treats the existence of a
  merged PR row as proof that its reviews, check-runs, and branch commits are
  fully synced. Writing the PR first opened a crash window: the PR row committed
  but its reviews never landed, and every later run skipped the reviews fetch —
  permanently reporting "no approval on final commit." With the PR last, a crash
  leaves orphan sub-rows that the next run re-fetches.
- **PR-branch commits insert-if-absent, never upsert.** `/pulls/{n}/commits`
  rows lack `author_email`, `href`, `is_verified`, and diff stats. A blind
  upsert replaced rich phase-1 rows with gutted copies, breaking the §1
  `verified_emails` fallback and merge classification on later DB reads.

### Phase 3: Audit

Each unaudited commit is evaluated by `EvaluateCommit()` using the enrichment
data. Results are written to `audit_results`.

### Phase 4: Cursor update

The cursor records the branch tip SHA (drives the next run's graph compare) and
the newest committer date seen (the date-window fallback resume point). So the
next sync picks up where this one left off.

## Database schema

DuckDB, 10 tables:

| Table | Primary Key | Purpose |
|---|---|---|
| `sync_cursors` | (org, repo, branch) | Incremental sync progress (`last_sha` tip for graph compare + `last_date` watermark) |
| `commits` | (org, repo, sha) | Git commits from GitHub. `files_changed` + `detail_fetched_at` record verified commit detail: NULL `detail_fetched_at` means "never fetched", letting verified-zero stats survive as facts. Stat-less re-ingestion (cursor-overlap re-lists) preserves verified detail via a staging-table pre-merge UPDATE. |
| `co_authors` | (org, repo, sha, email) | Co-authors parsed from "Co-authored-by:" trailers |
| `commit_branches` | (org, repo, sha, branch) | Which branches a commit appears on |
| `commit_prs` | (org, repo, sha, pr_number) | Commit → PR associations |
| `pull_requests` | (org, repo, number) | GitHub pull requests |
| `reviews` | (org, repo, pr_number, review_id) | PR reviews with per-reviewer state |
| `check_runs` | (org, repo, commit_sha, check_run_id) | CI/CD check results |
| `audit_results` | (org, repo, sha) | Compliance verdicts with reasons |
| `org_repos_cache` | (org, name) | Memoised `/orgs/{org}/repos` enumeration (freshness-gated) |

**Bulk writes** use the DuckDB Appender API (staging table → merge). The merge
is `INSERT OR REPLACE` on the fast path. When the target row has non-empty LIST
columns, DuckDB raises "List Update is not supported" and the merge falls back to
delete-colliding-rows + insert (two separate statements — DuckDB's ART index
rejects a same-key delete+insert in one transaction). Intra-batch duplicates
dedupe deterministically, last-wins (`ROW_NUMBER … ORDER BY rowid DESC`). All
writes go through a serialized `DBWriter`: DuckDB allows concurrent reads but a
single writer.

**Text, not enums.** `reviews.state` and `check_runs.status`/`conclusion` are
TEXT. GitHub returns `PENDING` for the caller's own draft reviews and may add
states; one un-castable value used to hard-fail the whole batch. `UpsertReviews`
filters `PENDING` (drafts are not audit events); unknown states are stored as-is.
Commit read paths scan nullable columns through `sql.Null*`, so legacy rows (e.g.
a NULL `committer_login` predating that column) can't brick reads on upgraded
databases.

## Token pool

The pool manages a mixed set of GitHub credentials. Two kinds:

| Kind | Config fields | Auth mechanism |
|------|---------------|----------------|
| **PAT** (`kind: pat`) | `env` (env var name) | Bearer token header |
| **App** (`kind: app`) | `app_id`, `installation_id`, `private_key_path` or `private_key_env` | JWT → installation access token via [ghinstallation](https://github.com/bradleyfalzon/ghinstallation); auto-refreshes before expiry |

Each token carries **scopes** (`org` + optional `repos`) that limit which
org/repo pairs it may serve. Scope matching is case-insensitive; an empty repos
list means all repos in that org.

Auto-detection (when no tokens are configured): `GH_TOKEN` → `GITHUB_TOKEN` →
`gh auth token`. The first found becomes a wildcard token with no scope limit.

For the minimum token permissions, see [Required
permissions](README.md#required-permissions). gh-audit is read-only.

### Rate limit handling

- Tracks `x-ratelimit-remaining` and `x-ratelimit-reset` from response headers.
- Scores each token by `rateRemaining - inFlight` and picks the highest. The
  in-flight counter stops concurrent `Pick` calls from herding onto one token
  before any response lands.
- Blocks and waits for reset when all matching tokens are exhausted (threshold:
  100 remaining).
- Retries on 429. Respects `Retry-After` (delta-seconds and HTTP-date forms;
  defaults to 60s). A 429 whose body signals the secondary rate limit cools the
  token and re-picks, like the 403 path.
- Detects 403 abuse / secondary rate-limit responses. The generic-403 one-shot
  retry re-classifies its response, so an abuse 403 on retry still cools the
  token.
- Header updates are monotonic per token (mutex + ignore out-of-order
  responses), so a stale response can't resurrect an exhausted token. Selection
  tolerates negative scores and honors ctx cancellation instead of busy-spinning.
- A global in-flight cap (counting semaphore, default 300) bounds concurrent
  HTTP requests across the whole pool, so pipeline fan-out can't trip GitHub's
  ~480-concurrent secondary-rate-limit ceiling.
- Repeat secondary-rate-limit trips escalate the token's cooldown (90s → 15m),
  clamped to the hourly primary reset.
- `MarkDisabled` permanently removes a token from rotation (for credential
  failures such as 401).

## Caching layer

Enrichment goes through `CachingEnricher` (`internal/github/caching.go`),
between the sync pipeline and the raw REST `Client`. It keeps enrichment
idempotent and cheap: a second `sync`, a `re-audit`, or `backfill-missing-prs`
should not re-fetch data already on disk.

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
               (CommitDetailEager / CommitDetailLazyEmpty / CommitDetailLazySelf /
                CommitPRs / PRDetail / Reviews / CheckRuns / PRCommits /
                RevertVerification)
```

Key design points:

- **Reverse PR index.** A PR fetched for commit A may also be the merge PR for
  commit B. `indexPR` populates a reverse map, so B's enrichment finds A's PR
  work without a second round-trip.
- **Lazy commit detail.** Phase-1 `commits` rows already carry most of what the
  audit needs. `GetCommitDetail` is called only when the tree needs stats (the
  empty-commit fallback) — saving roughly 16% of REST traffic on a typical run.
- **Merged-PR freeze.** Sub-data of a merged PR already in the DB (reviews,
  check runs, PR commits) is frozen and not re-fetched. The freeze requires the
  PR row to actually be merged: rows snapshotted for a non-merged PR are a
  moment-in-time copy and always refetch, however many rows exist. Two
  carve-outs:
  - check-run rows are authoritative only when every run is `status=completed`
    (in-flight runs persisted minutes after a merge would otherwise cache
    "missing" forever);
  - the freeze knowingly does not observe post-merge review changes (dismissals)
    after the first sync — re-sync is required for that.

  The pipeline writes the PR row last, so the freeze can't trust a
  half-persisted PR.
- **Fan-out bounds.** `enrichCommitFanout = 10` (per batch) and `enrichPRFanout
  = 5` (per commit) cap goroutine growth without flooding the token pool.
- **Revert-verification telemetry.** `GetCommitFiles` calls made to diff-verify
  manual reverts are tracked separately in `APIStats.RevertVerification` — the
  most expensive per-commit call, worth watching on its own.

## Revert & merge classification

Two small classifiers feed the audit tree and the XLSX report.

### `ParseRevert` (`internal/github/revert.go`)

| Kind | Trigger | Clean? |
|---|---|---|
| `NotRevert` | Message has no recognised revert prefix | — |
| `AutoRevert` | `Automatic revert of <new>..<old>` | **Yes** (trusted by construction) |
| `ManualRevert` | `Revert "..."` prefix; the reverted SHA comes from the `This reverts commit <sha>.` trailer, or — for GitHub's "Revert" button, which omits the trailer — from the reverted PR's `merge_commit_sha` via the `revert-<N>-<base-branch>` head-branch convention (`ResolveRevertedSHA`) | **Only if** `IsCleanRevertDiff` confirms the diff is the exact inverse of the reverted commit |
| `RevertOfRevert` | Revert-of-revert (re-application) | No — treated as fresh code |

`IsCleanRevertDiff` compares file patches as multisets of added/removed lines;
order is ignored. Parsing is hunk-aware: `+++ `/`--- ` count as file headers
only before the first `@@`, so content lines that begin with `++` or `--`
(C-style increments, diff-in-diff) are compared as real lines, not dropped. A
`ManualRevert` that fails the diff check becomes `revert_verification =
"diff-mismatch"` (or `"message-only"` when no trailer SHA was found). It does
**not** qualify for rule 8 — it falls through to the normal PR-approval rules.

`GetCommitFiles` paginates `files[]` to GitHub's 3,000-file ceiling. A commit
that hits the ceiling returns `ErrCommitFilesTruncated` and classifies as
`message-only` (unverifiable), never `diff-verified` — a truncated comparison
could otherwise "verify" a revert that smuggles changes into files past the cut.

### `ClassifyMerge` (`internal/github/merge.go`)

| Kind | Parents | Extra signals |
|---|---|---|
| `NotMerge` | 0–1 | — |
| `CleanMerge` | 2 | `Merge pull request #…` message AND `committer_login == web-flow` AND `is_verified == true`. All three required. |
| `DirtyMerge` | 2 | Any missing signal — non-matching message, non-web-flow committer, or unverified signature. Could hide committer-authored code. |
| `OctopusMerge` | 3+ | Rare; usually tooling-generated. Not auto-trusted. |

The `CleanMerge` signal is deliberately strict. Message-only matching is
forgeable — anyone can craft a `Merge pull request #…` commit locally. The
`web-flow` committer plus a GitHub-verified signature is what makes it
trustworthy: only GitHub holds the web-flow signing key, so the signal can't be
produced outside GitHub's merge button.

`is_verified` is read from the REST API's `commit.verification.verified` field
(on both `GET /commits/{sha}` and `GET /repos/{o}/{r}/commits`). It is persisted
in the `commits` table, so the DB-read path preserves it.

These flags drive the **Waivers Log** and **Decision Matrix** sheets, the rule-8
fallback, and the §5 CleanMerge exclusion. They are **informational for
compliance**, except `IsCleanRevert`, which rule 8 turns into a standalone
waiver (the reverted commit's own verdict is not consulted — see `TODO.md`).

### `classifyMergeStrategy` (`internal/sync/audit.go`)

An informational label on every `audit_results` row. Does not affect compliance.

| Strategy | Detection | Typical source |
|---|---|---|
| `initial` | `parent_count == 0` | Repository root commit |
| `merge` | `parent_count > 1` | GitHub's "Create a merge commit" button |
| `squash` | 1 parent, has PR, `committer_login == web-flow` | GitHub's "Squash and merge" button |
| `rebase` | 1 parent, has PR, `committer_login != web-flow` | GitHub's "Rebase and merge" (fast-forward) |
| `direct-push` | 1 parent, no PR | `git push` without a pull request |

**Ambiguity.** Non-fast-forward rebase merges also get `committer=web-flow`
(GitHub replays the commits), so they look like squash merges at the commit
level. Feature-branch commits visible on main via a 2-parent merge also show as
`rebase`, since their original committer is preserved.

## Annotations

`internal/sync/annotations.go` computes informational annotations from each
commit's message. They are attached to every `audit_results` row, whatever the
compliance path, and are **not** load-bearing for compliance today.

- `detectAutomationTag` flags automation / dep-bump markers (Dependabot,
  Renovate, etc.), so auditors can cross-check against exempt-author config.

The `annotate-commits` CLI recomputes these for every existing row without
hitting the API — useful after adding a new detector.

## Report layer

The `report` command queries `audit_results` joined with `commits` and
`pull_requests`. Four output formats:

- **table** — ASCII summary + details to stdout.
- **csv** — per-commit rows with all fields.
- **json** — `{ summary: [...], details: [...] }`.
- **xlsx** — an 8-sheet workbook, layered Action → Overview → Trace/Evidence.
  Each sheet has one distinct purpose; a commit is never split across sheets.

**Layer 1 — Action**

1. **README** — legend for rule codes (R1..R8), cell values, and the report
   period. Static; one-screen orientation for new auditors.
2. **Action Queue** — prioritized commits needing action. Rows are non-compliant
   commits with no waiver (R1 exempt / R2 empty; an R8 clean-revert tag only
   waives when the pipeline already folded it into a compliant verdict). Sorted
   by severity desc, then org/repo, then commit date desc. Columns: Priority,
   Severity, Repo, SHA, PR #, Author, Merged By, Failing Rule, Prescribed
   Action, Context, Committed, Days Since Commit, Resolution, Notes. Severity and
   action come from `SynthesizeAction` (`internal/report/rules.go`); Context is
   the secondary fact pattern from `SynthesizeContext` (self-merged, merge
   strategy, failed revert classification, etc.).

**Layer 2 — Overview (filterable totals)**

3. **Summary** — per-repo rollup, `Total = Compliant + Non-Compliant`. Also:
   waived (R1/R2/R8 + clean-merge), per-rule fire counts (R3 NoPR, R4 NoFinal,
   R6 OwnerFail), and informational tags (Self-Approved, Stale, Post-Merge, Clean
   Reverts, Clean Merges, Bots, Exempt, Empty, Multiple PRs). Compliance % is
   color-coded; the TOTAL row carries SUM/IF formulas.
4. **By Rule** — triage pivot, one row per rule (R1..R8): fires, compliant vs
   non-compliant, waived, top repo, top author. Answers "which rule drags the
   fleet?".
5. **By Author** — per-author rollup (Commits / Non-Compliant / Self-Approved /
   Stale / Post-Merge / Compliance %). Sorted by non-compliant desc. A
   coaching/pattern view.

**Layer 3 — Trace & Evidence**

6. **Decision Matrix** — one row per commit, one column per rule. Cells are
   `pass` / `fail` / `skip` / `n/a` / `missing` / `waived`, color-coded. Freezes
   the first 3 columns (Repo / SHA / PR #) so rule columns scroll against fixed
   identity. Autofilter any rule column for a per-rule drill-down — this replaces
   the old dedicated Self-Approved / Stale / Post-Merge / Clean Reverts / Clean
   Merges sheets.
7. **Waivers Log** — one row per waiver tag (exempt-author / empty-commit /
   clean-revert / clean-merge / bot) with the evidence behind the skip.
   Clean-revert, clean-merge, and bot rows appear only when the stored verdict is
   compliant — the log is evidence of what the tool did NOT flag and why, so
   non-compliant commits never appear here.
8. **Multiple PRs** — one row per commit-PR pair for commits with `pr_count > 1`.

Decision Matrix outcomes are derived by `DeriveRuleOutcomes`
(`internal/report/rules.go`) from the stored `audit_results` booleans — no extra
SQL. The derivation mirrors the audit order in `internal/sync/audit.go` (R1 → R2
→ R3 → R4 → R5 → R6 → R7 → R8); any change to the audit logic must be reflected
there.

## Package structure

```
cmd/
  root.go                    Cobra root + flag wiring
  sync.go                    `sync` — fetch + enrich + audit (the main loop)
  report.go                  `report` — table / csv / json / xlsx output
  config.go                  `config validate` / `config show` — validate config file, print resolved config
  reaudit.go                 `re-evaluate-commits` (alias `re-audit`) — re-evaluate audit_results from DB (no API, single pass)
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
    report.go                Summary/detail/by-author queries, table/csv/json formatting
    rules.go                 DeriveRuleOutcomes + SynthesizeAction — per-commit rule trace and action synthesis
    xlsx.go                  8-sheet XLSX generation (README, Action Queue, Summary, By Rule, By Author, Decision Matrix, Waivers Log, Multiple PRs)
  sync/
    pipeline.go              Orchestration (discover → fetch → enrich → audit)
    audit.go                 EvaluateCommit decision tree (rules 1–8)
    annotations.go           ComputeAnnotations — informational flags from commit messages
    dbwriter.go              Serialized write channel for DuckDB
    progress.go              Sync phase tracking
```

## Concurrency model

- **Repo sync** — `concurrency` goroutines via `errgroup` (default 32). Branches
  within a repo fetch at the same limit, but each repo's enrich+audit phase is
  serialized across branches (`auditMu`).
- **Enrichment** — `enrich_concurrency` batch goroutines per repo (default 16);
  each batch fans out across commits (≤10) and PRs (≤5).
- **Audit** — ≤16 concurrent `EvaluateCommit` calls per repo
  (`auditFanoutLimit`), so the lazy `GetCommitDetail` paths (§2/§5) don't
  serialize.
- **DB writes** — a single `DBWriter` goroutine per run; all writes serialized
  through a buffered channel.
- **DB reads** — safe to run concurrently (DuckDB MVCC).

## Rate limits

GitHub REST API: 5,000 → 15,000 requests/hour per token (PAT or App). Cost per
commit: about 5 requests (PRs list + PR detail + reviews + check runs + PR
commits; commit detail is lazy). One token audits about 1,000 commits/hour.
Multiple tokens multiply throughput linearly — the pool routes each request to
the least-loaded scoped token. See [Token pool](#token-pool).
