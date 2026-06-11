#!/usr/bin/env bash
# bench.sh — run gh-audit's benchmark suite with consistent flags.
#
# Usage:
#   ./scripts/bench.sh                 # one pass, all benchmarks
#   ./scripts/bench.sh -count=5        # extra go test -bench flags pass through
#   BENCH=UpsertCommit ./scripts/bench.sh   # filter by name
#
# Compare two runs (requires golang.org/x/perf/cmd/benchstat):
#   ./scripts/bench.sh -count=10 | tee old.txt
#   ... make changes ...
#   ./scripts/bench.sh -count=10 | tee new.txt
#   benchstat old.txt new.txt
#
# Coverage map:
#   internal/sync    EvaluateCommit (single/multi-PR), review-state folding,
#                    annotations — the per-commit CPU kernel of the audit.
#   internal/github  clean-revert diff verification (300-file), revert/merge
#                    message classification — the enrichment CPU hot spots.
#   internal/db      bulk upserts incl. the LIST-column fallback path,
#                    batched vs per-row commit-PR links, sha-scoped lookups —
#                    the DBWriter-serialized write path.
#   internal/report  Summary/Details SQL at 50k rows, full XLSX build at 10k,
#                    per-row rule derivation — the reporting path.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FILTER="${BENCH:-.}"

cd "$REPO_ROOT"
go test -run '^$' -bench "$FILTER" -benchmem "$@" \
    ./internal/sync/ ./internal/github/ ./internal/db/ ./internal/report/
