# Auditing with gh-audit — an external auditor's guide

This is the practical procedure for using `gh-audit` as evidence in an
external audit (SOC 2 change management, SLSA Source Track, NIST SSDF).
For the internal design and the machine-checked soundness model, see
[Architecture.md](Architecture.md) and [tla/](tla/README.md).

---

## 1. What gh-audit attests — and what it does not

**Attests (per commit on a protected branch):**

- The commit traces to an approved pull request (§3/§7).
- The approval was **independent** — not self-approval by a code
  contributor (§4/§5), matched on immutable account **ids**.
- Required status checks passed on the merged code — **latest run wins**,
  so a stale green cannot mask a red re-run (§6).
- Waivers (exempt bot author, empty commit, clean revert) rest on
  **non-forgeable** evidence, or are flagged (§1/§2/§8).

**Detects (per branch):**

- **History rewrites** — a force-push that orphaned previously-audited
  commits (SLSA prohibits non-fast-forward moves).

**Does NOT attest:**

- That branch-protection *rules* were configured or enforced. gh-audit
  audits the **actual commits**, which is stronger than trusting a
  rule's presence — but it means it cannot tell you who was on a
  ruleset's bypass list. Cross-check the org's ruleset config and
  audit-log bypass events separately.
- CODEOWNERS path-specific review requirements.
- Anything about code *content* (it audits process, not correctness).

---

## 2. The audit procedure

```bash
# 1. Configure — see config.yaml (audit_rules + exemptions). The
#    audit-defining settings are hashed into the report's config
#    fingerprint, so the auditor can confirm which rules were applied.

# 2. Sync — fetch + audit every protected-branch commit. Stamps a
#    provenance record (build + config fingerprint) into the DB.
gh-audit sync --config config.yaml --db audit.db

# 3. Report — produce the evidence. JSON is the machine-readable,
#    attributable, tamper-evident artifact; xlsx is the human workbook.
gh-audit report --config config.yaml --db audit.db --format json  > report.json
gh-audit report --config config.yaml --db audit.db --format xlsx --output report.xlsx

# 4. Verify — independently confirm the report was not altered.
gh-audit verify --config config.yaml --db audit.db --manifest report.json
#   MATCH  → exit 0 : the report's verdicts match the database
#   MISMATCH → exit 1 : do not rely on the report
```

Every report leads with a **provenance manifest** (a header in `table`, a
`Provenance` sheet in `xlsx`, a `manifest` object in `json`). Read it
first.

---

## 3. Reading the provenance manifest

| Field | What it tells you |
|---|---|
| `tool_version` | The exact build that produced the verdicts (VCS revision or release). |
| `config_fingerprint` | SHA-256 of the audit-defining config (signing policy, review scope, audited branches, required checks, exempt account ids). Confirms *which rules* were applied. |
| `config_drift` | `true` = the report was generated under a **different** config than the one that computed the verdicts. Re-audit before relying on it. |
| `results_digest` | SHA-256 over the ordered verdict rows. `gh-audit verify` recomputes it; a mismatch means tampering. |
| `scope` | Repos, commit count, and date range covered. |
| `coverage` | The caveats — see §4. |

A DB that predates provenance stamping shows "unknown" attribution but
still carries a valid digest, scope, and caveats.

---

## 4. Interpreting the coverage caveats

The manifest's `coverage` block is the **honest disclosure** of what the
verdicts do not fully guarantee. Weigh each before signing off:

| Caveat | Meaning | What to do |
|---|---|---|
| `non_compliant` | Commits that failed the audit. | The primary finding — review the Action Queue. |
| `history_rewrites` | Force-pushes detected on a protected branch. | **Serious.** Previously-audited history was rewritten; the audit of that branch cannot be trusted at face value. Investigate. |
| `forgeable_exemptions` | §1 waivers that rest on the *unsigned* author-id hint. | These would fail under `signing_policy: required`. If the org claims to enforce signing, this should be 0. |
| `unresolved_author_ids` | Commits whose author id GitHub could not bind to an account (id 0). | Identity rules fall through for these; they are audited by review rules, not exemption. |

---

## 5. The trust model in one paragraph

A commit's `author` is a **hint the committer typed** (`git commit
--author=…`), forgeable on unsigned commits — even to a specific
account, via GitHub's `<id>+name@users.noreply.github.com` noreply form.
The only identity a commit carries that a client cannot forge is a
**verified signature**, which binds the committer (or, for GitHub
web-flow commits, GitHub-attests the author). gh-audit's §1 exemption
therefore anchors on the verified signer; the forgeable author path is
allowed only under `signing_policy: optional` (the default) and is
flagged as a forgeable exemption. Set `signing_policy: required` for a
provably-sound §1. Review/approval identities always come from
authenticated actions and are non-forgeable.

Each rule's soundness is machine-checked in [tla/](tla/README.md):
`Compliant ⇒ TrulySafe` over every interleaving of attacker moves, with
JVM-free conformance tests tying the specs to the Go code.

---

## 6. Framework mapping

| Control | Framework | gh-audit evidence |
|---|---|---|
| Two-person review of protected-branch changes | SLSA Source Track L4; NIST SSDF PS.1.1 | §3/§4/§5 — approved PR + independent (non-self) review, id-matched |
| Change tracking with per-account accountability | NIST SSDF PS.1.1 | verdicts keyed on immutable account ids |
| History immutability (no `--force`) | SLSA Source Track | history-rewrite detection |
| Required checks / quality gates | SOC 2 change management | §6 — latest-run-wins required checks |
| Attributable, tamper-evident audit record | SLSA Source Provenance; NIST SSDF PS.3.1 | provenance manifest + results digest + `verify` |

---

## 7. Known limitations

- **Keyless (gitsign/Sigstore) signatures** are not marked Verified by
  GitHub, so gh-audit treats them as unsigned. Keep `signing_policy:
  optional` for such repos, or supply an external attestation.
- **Ruleset bypass lists** are not surfaced. gh-audit audits actual
  commits (more robust than trusting rule presence), but an auditor
  should still enumerate who can bypass protections and review audit-log
  bypass events.
- **CODEOWNERS** path-specific requirements are not evaluated.
- **History-rewrite detection** needs a prior recorded head; a rewrite
  that happened before gh-audit first synced a branch cannot be detected
  retroactively.
- **Audit-record retention** is the operator's responsibility — the
  `audit_runs` log records each run's build + config, but gh-audit does
  not itself provide WORM storage or sign the manifest.
