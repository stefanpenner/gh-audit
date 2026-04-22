#!/usr/bin/env bash
# test-fixtures.sh — sync stefanpenner/gh-audit-test-fixtures and validate
# each scenario's audit verdict against the `assertions` array in that
# repo's scenarios.json.
#
# Usage (local):
#   GITHUB_TOKEN=$(gh auth token) ./scripts/test-fixtures.sh
#
# Usage (CI):
#   scripts/test-fixtures.sh
#   (the Actions default GITHUB_TOKEN covers public repo reads)
#
# Exits non-zero on any scenario whose actual audit outcome doesn't match
# the expected tuple.
#
# Dependencies: go, curl, jq, duckdb (CLI).
set -euo pipefail

FIXTURES_REPO="${FIXTURES_REPO:-stefanpenner/gh-audit-test-fixtures}"
SCENARIOS_URL="${SCENARIOS_URL:-https://raw.githubusercontent.com/${FIXTURES_REPO}/main/scenarios.json}"

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
    echo "error: GITHUB_TOKEN is required" >&2
    exit 2
fi

for bin in go jq duckdb curl; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        echo "error: required binary '$bin' not found in PATH" >&2
        exit 2
    fi
done

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cat > "$TMPDIR/config.yaml" <<EOF
database: $TMPDIR/audit.db
tokens:
  - kind: pat
    env: GITHUB_TOKEN
    scopes:
      - org: ${FIXTURES_REPO%%/*}
        repos: [${FIXTURES_REPO#*/}]
EOF

echo "==> Building gh-audit"
(cd "$REPO_ROOT" && go build -o "$TMPDIR/gh-audit" .)

echo "==> Syncing $FIXTURES_REPO"
"$TMPDIR/gh-audit" --config "$TMPDIR/config.yaml" sync \
    --repo "$FIXTURES_REPO" \
    --db "$TMPDIR/audit.db" \
    --telemetry-output=- \
    >"$TMPDIR/sync.log" 2>&1 || {
        echo "sync failed; tail of log:" >&2
        tail -n 40 "$TMPDIR/sync.log" >&2
        exit 1
    }

echo "==> Fetching scenarios.json"
curl -fsSL "$SCENARIOS_URL" > "$TMPDIR/scenarios.json"

echo "==> Validating assertions"
fails=0
total=0

# Each row: scenario-id<TAB>sha<TAB>expected_is_compliant<TAB>expected_is_clean_revert<TAB>expected_revert_verification
while IFS=$'\t' read -r id sha want_compliant want_clean want_rv; do
    [[ -z "${sha:-}" ]] && continue
    total=$((total + 1))

    row=$(duckdb -noheader -csv "$TMPDIR/audit.db" <<SQL
SELECT is_compliant, is_clean_revert, COALESCE(revert_verification, '')
FROM audit_results
WHERE sha = '$sha';
SQL
)
    if [[ -z "$row" ]]; then
        echo "FAIL $id  sha=${sha:0:12}  not found in audit_results"
        fails=$((fails + 1))
        continue
    fi

    got_compliant=$(cut -d, -f1 <<<"$row")
    got_clean=$(cut -d, -f2 <<<"$row")
    got_rv=$(cut -d, -f3 <<<"$row")

    if [[ "$got_compliant" == "$want_compliant" && "$got_clean" == "$want_clean" && "$got_rv" == "$want_rv" ]]; then
        printf 'OK   %s  sha=%s  compliant=%s clean=%s rv=%s\n' "$id" "${sha:0:12}" "$got_compliant" "$got_clean" "$got_rv"
    else
        printf 'FAIL %s  sha=%s\n' "$id" "${sha:0:12}"
        printf '     expected: compliant=%s clean=%s rv=%s\n' "$want_compliant" "$want_clean" "$want_rv"
        printf '     got:      compliant=%s clean=%s rv=%s\n' "$got_compliant" "$got_clean" "$got_rv"
        fails=$((fails + 1))
    fi
done < <(jq -r '
    .scenarios
    | to_entries[]
    | .key as $id
    | (.value.assertions // [])[]
    | [$id, .sha, .is_compliant, .is_clean_revert, .revert_verification] | @tsv
' "$TMPDIR/scenarios.json")

echo
if [[ "$fails" -gt 0 ]]; then
    echo "FAILED: $fails/$total assertion(s) mismatched"
    exit 1
fi
echo "PASSED: $total/$total assertions match"
