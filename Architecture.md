# Architecture

This is the design reference: how gh-audit decides, and why those decisions
can be trusted. For how to install, run, and configure it, see
[README.md](README.md).

## GitHub's data model

Every verdict names a person. GitHub records several different "people" on one
merged commit, and they are not the same person — nor equally trustworthy. This
section pins down each role once, so the rules below can just say "author id"
and mean something exact.

### The five roles

| Role | What it means | GitHub field | Trustworthy id? |
|---|---|---|:---:|
| **Author** | Who wrote the code | `commit.author` | **Yes** — `author.id`, when GitHub binds the commit's email to an account |
| **Committer** | Who created the commit object | `commit.committer` | **No id exposed** — login only (e.g. `web-flow` for web merges) |
| **PR author** | Who opened the pull request | `pull_request.user` | **Yes** — `user.id` |
| **Reviewer** | Who submitted a review | `review.user` | **Yes** — `user.id` |
| **Merger (actor)** | Who clicked merge | `pull_request.merged_by` | **Yes** — `user.id`, informational only |

### Two layers, one commit

A commit carries two layers of identity, and they differ in trust:

- **Git layer** — `author.name`, `author.email`, `committer.*`, and the message.
  These are set by the pushing client. They are **forgeable**: anyone can set
  any name, email, or date locally. The exception is content itself — SHAs and
  parent SHAs are content-addressed, so they can't be changed without changing
  the commit.
- **GitHub layer** — the numeric account ids GitHub adds when it recognises the
  git email (`author.id`, `committer.id` where present). GitHub sets these from
  a verified account. They are **immutable, never reused, and not forgeable by a
  client**. An unrecognised email yields `id == 0`; a deleted account collapses
  to the shared ghost sentinel `10137`.

The whole audit keys on the GitHub layer. The git layer is used only as a hint,
and only after a GitHub-set fact confirms it (see [Trust model](#trust-model)).

### Author vs committer — the trap

These two are different people far more often than they look:

- **Squash / rebase merges** — GitHub replays your commits. The **author** is
  preserved (the human who wrote the code); the **committer** becomes `web-flow`
  (GitHub's merge identity).
- **Merge-commit button** — `web-flow` is the committer; the merge commit has no
  meaningful single author.
- **Direct push** — author and committer are usually the same local user.

So "who wrote this" (author) and "who landed it" (committer/merger) are separate
questions. For review/approval identity (§4/§5) gh-audit answers with `author.id`
and reviewer ids from authenticated actions. For the §1 exempt waiver the
trustworthy anchor is instead the **verified committer** (`committer.id` gated on
`IsVerified`): a valid signature is the only thing binding a commit's identity to
an account, whereas `author.id` is resolved from a client-set email and is
forgeable (see §1). Committer *login* and `Co-authored-by` trailers remain
excluded from compliance entirely (mutable, unauthenticated — see §5).

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
3. **The operator-curated exempt list** (`exemptions.authors`: account **ids**
   only). This is a deliberate trust root. The operator vets which bot and
   service accounts may ship without human review. Matching is id-only — there
   is no email path, because a git-author email is forgeable (see §1).

The GitHub token is **read-only** (see [Required permissions](README.md#required-permissions)).
gh-audit never writes, so it cannot perturb its own evidence.

### What each verdict input rests on

| Signal | Source (endpoint) | Forgeable alone? | What makes it trustworthy | Feeds |
|---|---|:---:|---|---|
| `reviewer.id`, PR author id | `/pulls/{n}/reviews`, `/pulls/{n}` → `*.id` | **No** | Bound to the account that performed an authenticated action; `TrustedID` rejects 0/ghost | §4, §5 |
| commit `author_id` | `GET /commits/{sha}` → `author.id` | **Yes (unsigned)** | Resolved from a client-set git-author email — a noreply address forges any id on an unsigned commit. Only a hint; §1 gates it behind `signing_policy` | §1 (forgeable path) |
| commit `committer.id` + `verification.verified` | `GET /commits/{sha}` | **No** | A valid signature binds the committer to the signing account — the one non-forgeable identity a commit carries | §1 (sound path) |
| `review.state`, `commit_id`, `submitted_at` | `/pulls/{n}/reviews` | No | GitHub-recorded; tied to a head SHA the client can't choose | §4 |
| `dismissed_at`, `dismissed_state` | `/issues/{n}/events` | No | GitHub-recorded timeline event | §4 |
| commit **parent SHAs** | commit object | No | Content-addressed — a forged parent changes the commit's own SHA | §4 graph carve-out |
| `pr.merge_commit_sha` | `/pulls/{n}` | No | GitHub sets it atomically at merge time | §3 recovery, §8 manual-revert target |
| check-run conclusions | `/commits/{ref}/check-runs`, `/status` | No | GitHub/CI-reported | §6 |
| `web-flow` committer + `verification.verified` | commit detail | No | Only GitHub holds the web-flow signing key (CleanMerge) | §5 author-skip |
| revert diff (Auto + Manual) | `GetCommitFiles` | No | Actual patch multiset-compared against the reverted commit | §8 |
| exempt list (account **ids** only) | operator config | n/a (trust root) | Operator-curated, vetted, id-only (no email path) | §1, §4 carve-out |
| `(#N)` squash-message token | commit message | **Yes** | **Verified before trust** — see below | §3 recovery |
| revert-message prefix (Auto + Manual) | commit message | **Yes** | **Verified before trust** — only picks the SHA to diff against; see below | §8 |

### Leaps through forgeable nodes

A "leap" is trusting a forgeable signal to reach a verdict without first
anchoring it to something non-forgeable. The rule is absolute: **a forgeable
signal can flip a verdict to non-compliant, or stay advisory, but it can never
flip a verdict to compliant on its own.** Every forgeable hint that feeds a
waiver is first anchored to a canonical fact. Two forgeable inputs remain in the
pipeline; here is how each is handled.

- **§3 squash `(#N)` token — verified, no leap.** The `(#N)` in a message is a
  forgeable hint. It is accepted only when `pr.merged && pr.merge_commit_sha ==
  sha` (`recoverPRFromMergeMessage`). A message claiming `(#1234)` cannot make
  `pulls/1234.merge_commit_sha` equal this commit's SHA. Forgeable hint →
  non-forgeable check.

- **§1 exempt match — id-only, no forgeable input.** Exemption matches the
  immutable numeric `author_id` against the curated list and nothing else. The
  git-author email is **never** consulted: it is client-set and GitHub leaves
  `author_id == 0` when it can't bind it, so an email path would let any pusher
  forge an exemption. The retired `verified_emails` config key is rejected at
  load time (`config.go`). An unresolved id (0) or the shared ghost id is never
  exempt.

- **§8 reverts (Auto + Manual) — diff-verified, no leap.** A revert message
  (`^Automatic revert of <new>..<old>` or `Revert "…"` + `This reverts commit
  <sha>`) is a forgeable hint: it only names *which* commit is claimed to be
  reverted. The waiver fires only when `IsCleanRevertDiff` confirms the revert
  commit's own patch is the exact inverse of that commit's patch
  (`RevertVerification = "diff-verified"`, `verifyRevertDiff` in `caching.go`).
  A commit whose bytes are the exact inverse of `<sha>` *is* a clean revert of
  it — whoever authored it, whatever the message claims. Any unresolved or
  mismatched diff stays `message-only` / `diff-mismatch`, sets no waiver, and
  falls through to the normal PR-approval rules (so it can land non-compliant).
  AutoRevert used to be waived on the message alone; that leap is now closed —
  it carries the reverted SHA directly but is verified like any other revert.

Everything else that could be forged — committer login, `Co-authored-by`
trailers — is **excluded from compliance entirely** (see §5, "Excluded identity
sources"). The §4 graph carve-out replaced a forgeable committer timestamp with
positional graph ancestry. There is **no** timestamp fallback: rows synced
before parent SHAs were persisted have no non-forgeable ordering, so the
carve-out fails closed (no promotion) until one online re-sync persists parent
SHAs. A backdated `GIT_COMMITTER_DATE` can therefore never launder an
unreviewed post-approval commit into compliance.

### Chain-of-custody checklist

Every way a commit can become **compliant**, the non-forgeable fact that guards
it, and the regression test that proves a forged input is rejected. A human
auditor can walk this table top to bottom to confirm no waiver rests on a
forgeable signal.

| Path to compliant | Non-forgeable anchor | Forged input it rejects | Proving test |
|---|---|---|---|
| §1 exempt author | `author_id` == curated id (`TrustedID`, id-only) | git-author email / login | `TestEvaluateCommit_Rule1_IDOnlyExemption` ("FORGERY…"), `TestExemptCommit_IDOnly` |
| §2 empty commit | GitHub diff stats == 0/0/0 | — (no identity) | `TestEvaluateCommit_Rule2_EmptyCommit` |
| §3 PR recovery | `pr.merge_commit_sha == sha` | `(#N)` message token | `TestRecoverPRFromMergeMessage_MismatchRejected` |
| §4 approval on final | reviewer `id` (`TrustedID`), review on head SHA | reviewer login | `TestEvaluateCommit_Rule4_*` |
| §4 carve-out (refresh) | first-parent graph walk to approved SHA | committer timestamp (backdating) | `TestApprovalRefreshable_PositionalNotTemporal` ("LEAK…") |
| §5 no self-approval | id-only `sameUser` | login, committer, co-authors | `TestEvaluateCommit_Rule5_*` |
| §6 required checks | check-run conclusion (GitHub) | — (no identity) | `TestEvaluateCommit_Rule6_*` |
| §7 landing scope | PR `base.ref` ∈ audited branches (`prDelivers`) | sibling-branch review credit | `TestEvaluateCommit_Rule7_LandingScope` |
| §8 clean revert | diff is exact inverse (`IsCleanRevertDiff`) | revert commit message | `TestClassifyRevert_AutoRevertRequiresDiffVerification` |

If a new waiver is added, it must enter this table with its anchor and a
forgery-rejection test, or it does not ship.

Every waiver row is additionally **model-checked**. The TLA+ specs in
[`tla/`](tla/README.md) each play every interleaving of attacker moves
against the verdict logic and prove the soundness invariant — compliant
implies truly authorized/safe — over a bounded state space:

| Rule | Spec | Attack the red config rediscovers |
|---|---|---|
| §1 exempt | `Exempt.tla` | forged author id on an unsigned commit (retired author-id-only rule); `Exempt_amber.cfg` = the shipped `signing_policy: optional` tradeoff |
| §2 empty | `EmptyCommit.tla` | rename-only commit laundered by a lines-only check |
| §4/§5 approval | `Approval.tla` | backdated `GIT_COMMITTER_DATE` (retired timestamp carve-out) |
| §6 checks | `Checks.tla` | stale green run masking a red re-run |
| §7 landing | `Verdict.tla` | sibling-branch review credited for a protected-branch landing |
| §8 revert | `Revert.tla` | forged revert message (retired message-only AutoRevert) |

Each retired/naive rule is kept as a red config so TLC rediscovers its
attack — the machine-checked record of why each shipped rule is shaped
the way it is. Two `*_amber.cfg` configs run a *shipped* rule whose `Sound` TLC
violates on purpose, surfacing its residual assumption as a documented
tradeoff: `Verdict_amber` (fail-open on an unknown base) and
`Exempt_amber` (`signing_policy: optional` — the forgeable author-id
path, surfaced as **Weak Exempt** in the report). Bait configs
(`*_bait.cfg`) prove every green verdict is non-vacuous: a compliant
state must be reachable. Run `./tla/run.sh`; CI runs it on every PR.

The spec↔code link for §6 and §1 is machine-checked without a JVM:
`internal/sync/checks_spec_test.go` and `exempt_spec_test.go` replay
each spec's full bounded state space against the real
`evaluateRequiredChecks` / `exemptStatus` (the latter across both
signing policies).

The specs prove the rules we thought of. The `formal-gap-hunt` skill
hunts for the rest — real GitHub behaviour or orderings the specs cannot
yet express — and is meant to be rerun periodically (`tla/gaps/`).

### Verdict scope — landing vs content

The table certifies that no *forged* input flips a verdict. A separate,
non-forgery question is *where* the crediting review happened. By default §7 is
**landing-scoped**: a PR's approval counts only when the PR merged into an
audited branch (`prDelivers`, base-branch match). A review scoped to a sibling
branch (gitflow `feat → dev`) is genuine and non-forgeable, but it does not
vouch for a landing on `main`, so it cannot confer compliance. Operators who
want the older "reviewed anywhere" semantics set `audit_rules.review_scope:
content`. See [§7, "Scope of the verdict"](#7-compliance-verdict). Either way the
scope is a *policy* choice over genuine reviews — never a leap through a
forgeable node.

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

The match is **id-only** (against `ExemptAuthor.id`), but *which* id it anchors
on is the whole game, because **`author_id` is a hint the committer controls**.
GitHub resolves `commit.author_id` from the git-author email — and a noreply
email `<id>+name@users.noreply.github.com` resolves to `<id>` even on an
**unsigned** commit (`verification.verified == false`). So `git commit
--author=…` lets any pusher stamp any account's id onto the author field. The
only identity a commit can carry that a client *cannot* forge is a **verified
signature**, which binds the **committer** to the signing account
(`commit.committer.id` when `IsVerified`). (GitHub does expose a committer
account id — it just isn't trustworthy without the signature.)

So §1 has two paths, selected by `audit_rules.signing_policy`
(`exemptStatus` in `audit.go`):

- **Verified signer (sound).** `IsVerified && committer.id ∈ exempt` → exempt.
  A real signature proves the committer is the exempt account. Always allowed.
- **Author-id hint (forgeable).** `author_id ∈ exempt` on an unsigned/unverified
  commit → exempt **only under `signing_policy: optional`** (the default), and
  the verdict is tagged `trust:forgeable-exemption` (`ExemptionForgeable`) so a
  team can see which waivers rest on a forgeable node. Under `signing_policy:
  required` this path is closed — an unsigned commit claiming the bot is **not**
  exempt (fails closed). Teams that enforce commit signing opt into `required`
  for a provably-sound §1.

Signing is progressive enhancement: `optional` never breaks a team whose bot
doesn't sign (the waiver still fires, just flagged); `required` is the lock-down.
An unresolved id (0) and the shared ghost id never match on either path. A
verified-signer match is exempt even on a direct push — the identity is proven.

**Squash backstop.** A bot can squash human code into one commit. So the
exemption holds only when every PR-branch commit is also exempt by id
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

**Retired email path.** Earlier versions matched `commit.author_email` against a
curated `verified_emails` list when `author_id` was 0. That email is forgeable,
so the path was removed: `verified_emails` in config is now rejected at load
time with a migration message. Service accounts must be exempted by their
GitHub account id.

```
config.yaml: exemptions.authors[]    ← SOT: operator-curated list (account ids only)
      │
      ▼
GET /repos/{o}/{r}/commits/{sha}
      → commit.author_id              ← set by GitHub from verified email binding
      │
      ▼
EvaluateCommit (audit.go)
      exemptStatus(commit, signing_policy):
        IsVerified AND committer.id matches an exempt.id?  → exempt, sound
        author_id matches AND signing_policy==optional?    → exempt, forgeable-flagged
        else (incl. id==0/ghost, or required+unsigned)     → not exempt, continue to rule 2
      │
      ▼ (when exempt)
      hasNonExemptPRContributors():
        any branch commit not exempt by id? → grant IsExemptAuthor flag for visibility,
                                              but audit the squash content normally
        all branch commits exempt?          → IsCompliant=true, reason="exempt: configured author"
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
approval passes the **same `isExemptCommit` check §1 uses** (numeric id only —
no email path). The PR's own
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
approved SHA (force-push), a missing head, or rows with no parent data at all
(pre-upgrade syncs) all mean no promotion. There is no committer-timestamp
fallback — a forgeable timestamp must never decide compliance. One online
re-sync persists parent SHAs and re-enables the carve-out for legacy rows.

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

**Scope of the verdict — read this.** "Associated PR" is broader than "the PR
that delivered this commit to the audited branch." `GET /commits/{sha}/pulls`
returns *every* merged PR whose branch ever contained the commit, on *any* base
branch (§3 table). To stop a review scoped to one branch from vouching for a
landing on another, the verdict is **landing-scoped by default**
(`audit_rules.review_scope: landing`):

> a PR's approval counts for §7 only when the PR merged into an **audited
> branch** (`pr.base.ref` ∈ `audit_branches`).

The check is `prDelivers` (`internal/sync/audit.go`): the PR's `base_branch` is
glob-matched against the repo's audited branches. A PR that merged elsewhere
still shows in reports (it satisfies §3 "has PR") but cannot confer compliance;
the reason reads `approval is on PR #N, which merged into "<base>", not an
audited branch`.

What this closes. Gitflow example: commit C is reviewed and merged in a
`feat → dev` PR, then C reaches `main` with its SHA preserved — via a direct
push or an unreviewed merge. The `feat → dev` PR (base `dev`) no longer vouches
for C's landing on `main`; unless a PR that merged into `main` independently
approved C on its final commit, C reads **non-compliant**. This was previously a
scope gap (the approval is real — not a forgeable-node leap — but scoped to the
wrong branch).

`base_branch` is populated on sync from `pull_request.base.ref` and persisted
(`pull_requests.base_branch`). It **fails open on missing data**: a PR row
without a base branch (synced before the field existed, or a partial fetch) is
credited, so an offline re-audit of old rows never flips a legitimate verdict —
one re-sync populates the field and re-enables the check. The gap only opens on
POSITIVE evidence (a *known* base outside the audited set).

**Opt-out — content scope.** Set `audit_rules.review_scope: content` to restore
the legacy behaviour: any associated merged PR's approval counts, wherever it
merged. Some flows (e.g. reviewed `feat → dev` with automated `dev → main`
promotion) legitimately want this — the code *was* reviewed, just not at the
`main` landing. `sync` and `re-audit` honour the same setting, so the two never
disagree.

### Signing policy (§1)

`audit_rules.signing_policy` selects how §1 anchors an exemption (see §1 above):

- `optional` (default) — progressive enhancement. A verified signer on the
  exempt list is the sound path; an unsigned commit that merely *claims* an
  exempt author is still waived but tagged `trust:forgeable-exemption`.
- `required` — fail the forgeable author path closed: only a verified signer is
  exempt. For teams that enforce commit signing and want a provably-sound §1.

`sync` and `re-audit` honour the same setting, so verdicts never disagree.

### 8. Clean-revert waiver (standalone)

If the verdict so far is **non-compliant**, one last check runs. It runs even on
the §3 no-PR path, so a diff-verified clean revert pushed straight to the branch
is waived too. It is per-commit: it does not look at the reverted commit's own
verdict (see `TODO.md` for the deferred cross-commit variant).

A `IsCleanRevert=true` commit is **compliant**. The signal is set only by
`revert_verification = "diff-verified"` — for both `AutoRevert` and
`ManualRevert`, the revert commit's diff was confirmed to be the exact inverse
of the reverted commit. The revert message merely names which commit to diff
against; it never waives on its own (see the [Trust model](#trust-model)).

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
         → file diffs for clean-revert verification (Auto + Manual reverts)
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
  rows lack `href`, `is_verified`, and diff stats. A blind upsert replaced rich
  phase-1 rows with gutted copies, breaking merge classification and the §5
  empty-contribution check on later DB reads.

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
  reverts (auto and manual) are tracked separately in
  `APIStats.RevertVerification` — the most expensive per-commit call, worth
  watching on its own.

## Revert & merge classification

Two small classifiers feed the audit tree and the XLSX report.

### `ParseRevert` (`internal/github/revert.go`)

| Kind | Trigger | Clean? |
|---|---|---|
| `NotRevert` | Message has no recognised revert prefix | — |
| `AutoRevert` | `Automatic revert of <new>..<old>`; the first SHA is the reverted commit | **Only if** `IsCleanRevertDiff` confirms the diff is the exact inverse of that commit (the message alone never waives) |
| `ManualRevert` | `Revert "..."` prefix; the reverted SHA comes from the `This reverts commit <sha>.` trailer, or — for GitHub's "Revert" button, which omits the trailer — from the reverted PR's `merge_commit_sha` via the `revert-<N>-<base-branch>` head-branch convention (`ResolveRevertedSHA`) | **Only if** `IsCleanRevertDiff` confirms the diff is the exact inverse of the reverted commit |
| `RevertOfRevert` | Revert-of-revert (re-application) | No — treated as fresh code |

`IsCleanRevertDiff` compares file patches as multisets of added/removed lines;
order is ignored. Parsing is hunk-aware: `+++ `/`--- ` count as file headers
only before the first `@@`, so content lines that begin with `++` or `--`
(C-style increments, diff-in-diff) are compared as real lines, not dropped. An
`AutoRevert` or `ManualRevert` that fails the diff check becomes
`revert_verification = "diff-mismatch"` (or `"message-only"` when the reverted
SHA could not be resolved or its files could not be fetched). It does **not**
qualify for rule 8 — it falls through to the normal PR-approval rules.

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
