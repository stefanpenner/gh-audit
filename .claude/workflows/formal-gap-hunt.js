export const meta = {
  name: 'formal-gap-hunt',
  description: 'Hunt for gaps between the gh-audit TLA+ specs, the Go audit rules, and real GitHub behaviour',
  whenToUse: 'Periodically, to find soundness gaps the current specs cannot even express. Invoked by the formal-gap-hunt skill.',
  phases: [
    { title: 'Spec-vs-code' },
    { title: 'GitHub-probe' },
    { title: 'Verify' },
    { title: 'Synthesize' },
  ],
}

// args (all optional):
//   { repos: ["owner/name", ...] }  real repos to probe (default set below)
const repos = (args && args.repos && args.repos.length)
  ? args.repos
  : ['emberjs/ember.js', 'kubernetes/kubernetes', 'rust-lang/rust', 'stefanpenner/gh-audit']

// Each module pairs a TLA+ spec with the Go predicate(s) it abstracts.
// A gap is anything the spec assumes away that the Go code (or GitHub)
// actually has to deal with.
const MODULES = [
  { rule: '§4/§5', spec: 'tla/Approval.tla',    code: 'evaluatePR, isApprovalRefreshable, postApprovalByGraph, latestReviewStatesOnFinal, isSelfApproval' },
  { rule: '§1',    spec: 'tla/Exempt.tla',      code: 'isExemptCommit, hasNonExemptPRContributors, applyExemptAuthorRule' },
  { rule: '§8',    spec: 'tla/Revert.tla',      code: 'evaluateRevertCompliance, ParseRevert, verifyRevertDiff (internal/github/revert.go)' },
  { rule: '§2',    spec: 'tla/EmptyCommit.tla', code: 'applyEmptyCommitFallback' },
  { rule: '§6',    spec: 'tla/Checks.tla',      code: 'evaluateRequiredChecks' },
  { rule: '§7',    spec: 'tla/Verdict.tla',     code: 'prDelivers, EvaluateCommit PR loop, betterVerdict' },
]

// Real-GitHub shapes known to stress review-attribution audits. Each probe
// looks for live examples and asks whether gh-audit's rules + specs handle them.
const PROBES = [
  { key: 'force-push-after-approval', desc: 'PRs approved, then force-pushed / new commits added before merge (stale-approval carve-out surface)' },
  { key: 'cross-fork-pr',             desc: 'PRs whose head repo != base repo (fork), and how reviewer/author ids resolve' },
  { key: 'revert-chains',             desc: 'revert-of-revert and re-apply chains; conflict-resolved GH-UI reverts' },
  { key: 'bot-and-merge-queue',       desc: 'bot-authored commits, merge-queue / merge-group commits, squash merges mixing bot + human commits' },
  { key: 'gitflow-base-branches',     desc: 'PRs merged into non-default branches (feat->dev->main) — landing-scope surface' },
  { key: 'ghost-and-dismissal',       desc: 'reviews by deleted accounts (ghost id), dismissed reviews, reviews re-added after dismissal' },
]

const GAPS = {
  type: 'object',
  required: ['gaps'],
  properties: {
    gaps: {
      type: 'array',
      items: {
        type: 'object',
        required: ['id', 'rule', 'title', 'scenario', 'why_gap', 'severity'],
        properties: {
          id:       { type: 'string', description: 'short kebab-case slug' },
          rule:     { type: 'string', description: 'e.g. §4, §8, or cross-rule' },
          title:    { type: 'string' },
          scenario: { type: 'string', description: 'concrete input/sequence a reviewer could reproduce' },
          why_gap:  { type: 'string', description: 'why the current spec or code may miss or mishandle it' },
          evidence: { type: 'string', description: 'file:line, a real GitHub URL, or "none"' },
          severity: { type: 'string', enum: ['high', 'medium', 'low'] },
        },
      },
    },
  },
}

const VERDICT = {
  type: 'object',
  required: ['confirmed', 'reasoning'],
  properties: {
    confirmed:  { type: 'boolean', description: 'true iff a REAL gap survived refutation' },
    reasoning:  { type: 'string' },
    refuted_by: { type: 'string', description: 'the exact code/test/spec that already handles it, or ""' },
    severity:   { type: 'string', enum: ['high', 'medium', 'low'] },
  },
}

// ---- Phase 1: static spec-vs-code gap review, one agent per module ----
phase('Spec-vs-code')
const staticFindings = await parallel(MODULES.map(m => () =>
  agent(
    `You are auditing the gh-audit repo for SOUNDNESS GAPS in rule ${m.rule}.\n\n` +
    `Read the TLA+ spec ${m.spec} and the Go predicate(s): ${m.code} (in internal/sync/audit.go unless noted). ` +
    `Also read the matching section of Architecture.md.\n\n` +
    `A GAP is something the SPEC assumes away that the Go code or real GitHub actually must handle — an input shape, ` +
    `an ordering, an identity edge case, or a state the spec's variables cannot even represent. ` +
    `You are NOT looking for bugs the spec already proves absent; you are looking for reality the spec does not model.\n\n` +
    `For each gap: give a concrete reproducible scenario, why the current spec/code may miss it, and a file:line anchor. ` +
    `Be adversarial and specific. If you find none, return an empty list — do not invent.`,
    { label: `spec:${m.rule}`, phase: 'Spec-vs-code', schema: GAPS }
  ).then(r => (r && r.gaps ? r.gaps.map(g => ({ ...g, source: 'spec-review' })) : []))
))

// ---- Phase 2: real-GitHub scenario probes, one agent per shape ----
phase('GitHub-probe')
const probeFindings = await parallel(PROBES.map(p => () =>
  agent(
    `You are hunting REAL GitHub examples that could break gh-audit's review-attribution audit.\n\n` +
    `Scenario class: ${p.desc}\n\n` +
    `Use the gh CLI (gh api / gh search / gh pr) against these repos: ${repos.join(', ')}. ` +
    `Keep it to a few bounded queries (GitHub search is rate-limited to 30/min — do not loop). ` +
    `Find 1-3 concrete live examples (commit SHAs or PR URLs) of this shape. Then read internal/sync/audit.go ` +
    `and the tla/ specs and decide: does gh-audit's model actually handle this shape correctly, or is there a gap ` +
    `where a real commit could be marked compliant without genuine independent review (or vice-versa)?\n\n` +
    `Report only gaps you can ground in a real example URL. Put the URL in evidence. If everything you found is ` +
    `handled, return an empty list.`,
    { label: `probe:${p.key}`, phase: 'GitHub-probe', schema: GAPS }
  ).then(r => (r && r.gaps ? r.gaps.map(g => ({ ...g, source: `probe:${p.key}` })) : []))
))

// Barrier is justified here: we dedup across ALL finders before spending a
// verifier per candidate, and we want a stable numbered set for the report.
const candidates = [...staticFindings, ...probeFindings].flat().filter(Boolean)
const seen = new Set()
const unique = []
for (const g of candidates) {
  const k = `${g.rule}|${(g.title || '').toLowerCase().slice(0, 60)}`
  if (seen.has(k)) continue
  seen.add(k); unique.push(g)
}
log(`${candidates.length} raw candidates -> ${unique.length} unique; verifying`)

// ---- Phase 3: adversarial verification, refute-first ----
phase('Verify')
const verified = await parallel(unique.map(g => () =>
  agent(
    `Try to REFUTE this claimed gh-audit soundness gap. Default to confirmed=false unless you cannot refute it.\n\n` +
    `Rule: ${g.rule}\nTitle: ${g.title}\nScenario: ${g.scenario}\nWhy claimed a gap: ${g.why_gap}\nEvidence: ${g.evidence || 'none'}\n\n` +
    `Read the actual Go code in internal/sync/audit.go (and internal/github/, internal/model/) and the tla/ specs and the ` +
    `Go tests. A gap is REFUTED if existing code or a test already handles the scenario soundly, or the scenario cannot ` +
    `actually occur. It is CONFIRMED only if a real input could yield a wrong verdict (compliant when it should not be, ` +
    `or vice-versa) that no current code path prevents. Name the exact code/test that refutes it, or explain precisely why ` +
    `nothing does.`,
    { label: `verify:${g.id}`, phase: 'Verify', schema: VERDICT }
  ).then(v => ({ ...g, verdict: v }))
))

const confirmed = verified.filter(Boolean).filter(g => g.verdict && g.verdict.confirmed)
log(`${confirmed.length} of ${unique.length} candidates confirmed after refutation`)

// ---- Phase 4: synthesize a report ----
phase('Synthesize')
const report = await agent(
  `Write a concise markdown gap report for gh-audit's maintainer (who is dyslexic — short lines, bullets, ` +
  `simple words, one idea per line).\n\n` +
  `Confirmed gaps (JSON):\n${JSON.stringify(confirmed, null, 2)}\n\n` +
  `Also for context, the full candidate set before refutation was ${unique.length} items.\n\n` +
  `Structure:\n` +
  `1. One-line summary: how many confirmed gaps, worst severity.\n` +
  `2. A table: id | rule | severity | one-line gap.\n` +
  `3. Per confirmed gap: the scenario, why it is a gap, and a concrete next step ` +
  `(new TLA+ action / variable, new Go test, or doc note). If evidence has a real URL, keep it.\n` +
  `4. If zero confirmed gaps, say so plainly and note what was checked.\n\n` +
  `Do not pad. Rank most-severe first.`,
  { label: 'synthesize', phase: 'Synthesize' }
)

return { confirmedCount: confirmed.length, candidateCount: unique.length, report, confirmed }
