# Formal model — audit verdict soundness (§1–§8)

TLA+ specs that model-check the core trust-model rule for every waiver:

> a forgeable (or stale) input can never flip a verdict to compliant
> unless a non-forgeable anchor truly justifies it.

Where the [chain-of-custody checklist](../Architecture.md#chain-of-custody-checklist)
proves one hand-written forgery per row, TLC proves the rule against
**every interleaving** of attacker and GitHub actions, in a small world.

## Run it

```bash
./tla/run.sh            # every module, ~1 min
./tla/run.sh Approval   # one module
./tla/run.sh list       # list modules
```

Needs a JRE (>= 11). First run downloads `tla2tools.jar` into
`tla/.tools/` (gitignored). CI runs the full suite on every PR
(`tla` job in `.github/workflows/test.yml`).

## Modules

Each module models one waiver as a two-player game: an **adversary**
(the pushing client — forges what a client controls) and **GitHub /
honest parties** (facts a client cannot influence). Every state carries
a **ground-truth** view (what really happened, which no auditor sees)
and an **observable** view (what the API returns). `Compliant` is the
audit rule computed from the observable view only. The invariant is
always:

```tla
Sound == Compliant => TrulySafe / TrulyAuthorized
```

| Module | Rule | Green (shipped) | Red (attack found) | Non-forgeable anchor |
|---|---|---|---|---|
| `Approval` | §4/§5 approval + stale carve-out | positional first-parent walk | committer-timestamp carve-out → **backdating** | graph ancestry |
| `Exempt` | §1 exempt author | id-only match | `verified_emails` path → **forged git email** | GitHub-bound `author_id` |
| `Revert` | §8 clean revert | diff-verified inverse | message-only AutoRevert → **forged revert message** | actual patch multiset |
| `EmptyCommit` | §2 empty commit | zero lines **and** files | lines-only → **rename-only laundering** | GitHub file count |
| `Checks` | §6 required checks | latest run per check wins | any-run-passed → **stale green masks red re-run** | GitHub check-run conclusion |
| `Verdict` | §7 landing scope | base.ref ∈ audited branches | content scope → **sibling-branch credit** | GitHub-set `base.ref` |

Every red config is kept on purpose: it is the machine-checked record
of a hole the shipped rule closed, and it proves each model is strong
enough to find that class of bug. `run.sh` asserts green **holds** and
red **is violated** — a red config that stops finding its attack fails
the run just as loudly as a green that breaks.

## Bait configs — greens can't pass vacuously

`Sound == Compliant => TrulySafe` holds trivially if `Compliant` is
never reachable. Each `*_bait.cfg` runs the green bounds with the
invariant `Bait == ~Compliant`, which claims no compliant state
exists. TLC must **violate** it — the witness trace is a reachable
compliant state, proof the green verdict has content. `run.sh` treats
a bait that stops violating as a failure.

## Bounds checked

Green TLC at these bounds is a strong bug hunt, **not** a proof.
Counts from a full run (2026-07-05); `run.sh` prints them live.

| Module | Bounds (green) | Distinct states |
|---|---|---|
| `Approval` | MaxCommits=4, MaxTime=2, MaxReviews=2 | 953 |
| `Checks` | MaxRuns=3, MaxTime=2 | 1,640 |
| `EmptyCommit` | MaxCount=2 | 55 |
| `Exempt` | MaxCommits=3 | 2,221 |
| `Revert` | (booleans only) | 9 |
| `Verdict` | NPRs=2 | 129 |

## The amber config — a documented tradeoff, machine-checked

`Verdict_amber.cfg` runs the **actually-shipped** `prDelivers`, which
fails **open** on an unknown base (`pr.BaseBranch == ""` is credited, to
avoid mass-flipping legitimate verdicts on legacy/missing data). TLC
reports `Sound` violated — and the counterexample is reachable *only*
through the unknown-base state:

```
approved = <<FALSE, TRUE>>   realBase = <<main, dev>>   observedBase = <<main, ?>>
```

PR 2 is genuinely approved, really merged into `dev`, but its `base.ref`
is missing — so fail-open credits it for a `main` landing nobody
reviewed. This is not a bug to fix; it is the **residual assumption made
explicit**: fail-open is sound only if an unknown base is never
attacker-suppressible (it isn't — `base.ref` is GitHub-set and only
blank on un-resynced rows). The `landing` green config is the strict
rule that needs no such assumption.

## What this proves — and what it does not

- **Proves:** the *design* of each waiver admits no laundering sequence
  within the model bounds.
- **Does not prove:** that `internal/sync/audit.go` implements the
  design. That link is the Go test suite (the checklist's "Proving
  test" column). Keep each module's `Compliant` in sync with the
  corresponding Go predicate when the rule changes:

  For `Checks` that link is machine-checked, no JVM needed:
  `internal/sync/checks_spec_test.go` enumerates the spec's full
  green-bounds state space (820 run sequences) and asserts the real
  `evaluateRequiredChecks` matches the spec's `Compliant` on every
  state, plus a mutation subtest proving the harness still catches
  the retired any-run rule. Use it as the template when bridging the
  other modules.

  | Module | Go predicate |
  |---|---|
  | `Approval` | `evaluatePR`, `isApprovalRefreshable`, `postApprovalByGraph` |
  | `Exempt` | `isExemptCommit`, `hasNonExemptPRContributors` |
  | `Revert` | `evaluateRevertCompliance`, `verifyRevertDiff` |
  | `EmptyCommit` | `applyEmptyCommitFallback` |
  | `Checks` | `evaluateRequiredChecks` |
  | `Verdict` | `prDelivers`, `EvaluateCommit` PR loop |

## Finding NEW gaps

The specs encode the rules we *thought of*. A gap is a real GitHub
behaviour no spec models — an attack the model can't even express. The
`formal-gap-hunt` skill (see `.claude/skills/`) reruns an adversarial
spec-vs-code review and a real-GitHub scenario search to surface them,
and is meant to be rerun periodically: as the models get better, they
find more.
