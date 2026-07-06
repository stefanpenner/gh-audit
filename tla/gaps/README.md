# Gap-hunt log

Dated reports from the `formal-gap-hunt` skill — soundness gaps found
between the `tla/` specs, the Go audit rules, and real GitHub behaviour.

- One file per run: `YYYY-MM-DD.md`.
- Newest is the current state; older files are the history of what was
  found and (once fixed) closed.
- Before a new run, skim the latest so you rank NEW gaps, not repeats.

Rerun with the `formal-gap-hunt` skill. It gets better as the models do.

## Closed

- **2026-07-06 — headline gap #1 of the 2026-07-03 run (forgeable
  `author_id` on unsigned commits).** `Exempt.tla` previously *assumed*
  the attacker could never pick the exempt id — a false axiom that hid
  the attack. The spec now models `authorId` as forgeable and anchors §1
  on the verified signer (`committer_id` + `verified`). `Exempt_red`
  rediscovers the forged-author waiver; `Exempt_green`
  (`signing_policy: required`) is sound; `Exempt_amber` is the shipped
  `optional` tradeoff. Bridged to `exemptStatus` by
  `internal/sync/exempt_spec_test.go`. Fix: PR #8; spec: this change.
