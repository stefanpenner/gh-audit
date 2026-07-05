---
name: formal-gap-hunt
description: Hunt for soundness gaps in gh-audit's audit rules using the TLA+ specs plus a team of agents that adversarially review spec-vs-code and probe real GitHub for scenarios the specs cannot model. Use when the user wants to re-check the formal model for holes, find new audit gaps, or periodically re-run the gap hunt as models improve. Produces a dated report under tla/gaps/.
user-invocable: true
---

# formal-gap-hunt — find holes the specs cannot yet express

The `tla/` specs prove the audit rules we *thought of* are sound. This
skill hunts for what they miss: real GitHub behaviour, orderings, or
identity edge cases the specs cannot even represent, and places where
`internal/sync/audit.go` diverges from its spec.

It is meant to be **rerun periodically**. As the models get better, the
same harness finds more. Each run is dated, so gaps accumulate as a log.

## Steps

### 1. Confirm the specs still pass

```bash
./tla/run.sh
```

Every module must print `ok` (green HOLDS, red/amber VIOLATED). If a
**green** flipped to VIOLATED, the shipped rule regressed — stop and
report that first; it is more urgent than any new gap. If a **red**
flipped to HOLDS, the model lost its teeth (someone weakened it) — also
stop and report.

### 2. Run the gap-hunt workflow

This is multi-agent orchestration — only run it because THIS skill was
invoked (that is the user's opt-in). Launch the named workflow:

- Call the `Workflow` tool with
  `{ name: "formal-gap-hunt", args: { repos: [...] } }`.
- `repos` is optional; default is a spread of complex-workflow repos.
  Pass the user's own audited repos when known (check CLAUDE.md's
  Verification section and `examples/`).
- It fans out: static spec-vs-code reviewers (one per rule) + real
  GitHub scenario probes, then refute-first verification, then a synthesis
  report. It returns `{ confirmedCount, candidateCount, report, confirmed }`.

If the user has NOT opted into multi-agent runs and you are unsure, do
the lighter single-agent version instead: read each `tla/*.tla` against
its Go predicate (mapping table in `tla/README.md`) and list gaps
yourself. Note in the report that it was the light pass.

### 3. Write the dated report

Save the workflow's `report` to:

```
tla/gaps/YYYY-MM-DD.md
```

Use today's date (check the environment's current date — do NOT call
`date` if a date is already provided in context). Prepend a short header:
run type (full workflow / light pass), repos probed, counts. Keep the
maintainer's dyslexia-friendly style: short lines, bullets, one idea per
line.

### 4. Turn confirmed gaps into work

For each confirmed gap, propose the concrete next step (do NOT implement
unless asked):

- **Modeling gap** (spec can't express it) → a new TLA+ action or
  variable in the relevant module, plus a red config that catches it.
- **Code/spec divergence** → a failing Go test (TDD) against the real
  predicate.
- **Accepted tradeoff** → a note, like `Verdict_amber.cfg`.

New waivers or anchors must also enter the Architecture.md
chain-of-custody table with a forgery-rejection test — that is the
repo's rule.

### 5. Summarize to the user

- Confirmed count + worst severity.
- The report path.
- Top 1-3 gaps, one line each, most severe first.
- Offer to implement the fixes.

## Notes

- The workflow script lives at `.claude/workflows/formal-gap-hunt.js`.
  Edit it to add scenario probes or modules as the audit grows; keep the
  `MODULES` list in sync with `tla/README.md`'s module→predicate table.
- GitHub search is rate-limited (30/min). The probe agents are told to
  stay bounded; if a run hits limits, rerun later or pass fewer `repos`.
- Prior reports in `tla/gaps/` are the memory of what was already found —
  skim the latest before acting so you rank NEW gaps, not repeats.
