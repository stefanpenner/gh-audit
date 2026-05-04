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
REPO_URL="https://github.com/${FIXTURES_REPO}"

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
echo "    $REPO_URL"
echo

fails=0
total=0
current_series=""

# Emit one JSON object per assertion with all fields from scenarios.json.
# Fields not present in the assertion are emitted as null (skipped during comparison).
while IFS= read -r assertion; do
    id=$(jq -r '.id' <<<"$assertion")
    title=$(jq -r '.title' <<<"$assertion")
    sha=$(jq -r '.sha' <<<"$assertion")
    comment=$(jq -r '.comment // empty' <<<"$assertion")

    [[ -z "$sha" || "$sha" == "null" ]] && continue
    total=$((total + 1))

    # Print series header when it changes
    series="${id%%.*}"
    if [[ "$series" != "$current_series" ]]; then
        current_series="$series"
        series_title=$(jq -r --arg s "$series" '
            .series[$s].title // empty
        ' "$TMPDIR/scenarios.json")
        if [[ -n "$series_title" ]]; then
            printf '\n  ── %s.x: %s ──\n\n' "$series" "$series_title"
        else
            printf '\n  ── %s.x ──\n\n' "$series"
        fi
    fi

    # Query all assertable fields from audit_results
    row=$(duckdb -noheader -csv "$TMPDIR/audit.db" <<SQL
SELECT is_compliant,
       is_clean_revert,
       COALESCE(revert_verification, ''),
       COALESCE(has_final_approval, false),
       COALESCE(is_self_approved, false),
       COALESCE(has_stale_approval, false),
       COALESCE(has_post_merge_concern, false),
       COALESCE(pr_count, 0),
       COALESCE(merge_strategy, ''),
       COALESCE(is_clean_merge, false)
FROM audit_results
WHERE sha = '$sha';
SQL
)
    if [[ -z "$row" ]]; then
        printf '  FAIL  %s — %s\n' "$id" "$title"
        printf '        %s/commit/%s\n' "$REPO_URL" "$sha"
        printf '        commit not found in audit_results\n'
        fails=$((fails + 1))
        continue
    fi

    IFS=',' read -r got_compliant got_clean got_rv got_final got_self \
                    got_stale got_concern got_prcount got_strategy got_cleanmerge <<<"$row"

    # Compare each field the assertion specifies (null = skip)
    mismatches=()
    check_field() {
        local field="$1" want="$2" got="$3"
        [[ "$want" == "null" ]] && return
        # Normalize booleans
        case "$want" in true) want="true" ;; false) want="false" ;; esac
        if [[ "$got" != "$want" ]]; then
            mismatches+=("$field: want=$want got=$got")
        fi
    }

    check_field "is_compliant"          "$(jq -r '.is_compliant // "null"' <<<"$assertion")"          "$got_compliant"
    check_field "is_clean_revert"       "$(jq -r '.is_clean_revert // "null"' <<<"$assertion")"       "$got_clean"
    check_field "revert_verification"   "$(jq -r '.revert_verification // "null"' <<<"$assertion")"   "$got_rv"
    check_field "has_final_approval"    "$(jq -r '.has_final_approval // "null"' <<<"$assertion")"     "$got_final"
    check_field "is_self_approved"      "$(jq -r '.is_self_approved // "null"' <<<"$assertion")"       "$got_self"
    check_field "has_stale_approval"    "$(jq -r '.has_stale_approval // "null"' <<<"$assertion")"     "$got_stale"
    check_field "has_post_merge_concern" "$(jq -r '.has_post_merge_concern // "null"' <<<"$assertion")" "$got_concern"
    check_field "pr_count"              "$(jq -r '.pr_count // "null"' <<<"$assertion")"               "$got_prcount"
    check_field "merge_strategy"        "$(jq -r '.merge_strategy // "null"' <<<"$assertion")"         "$got_strategy"
    check_field "is_clean_merge"        "$(jq -r '.is_clean_merge // "null"' <<<"$assertion")"         "$got_cleanmerge"

    if [[ ${#mismatches[@]} -eq 0 ]]; then
        printf '  OK    %s — %s\n' "$id" "$title"
        printf '        %s/commit/%s\n' "$REPO_URL" "${sha:0:12}"
        [[ -n "$comment" ]] && printf '        %s\n' "$comment"
    else
        printf '  FAIL  %s — %s\n' "$id" "$title"
        printf '        %s/commit/%s\n' "$REPO_URL" "$sha"
        [[ -n "$comment" ]] && printf '        %s\n' "$comment"
        for m in "${mismatches[@]}"; do
            printf '        ✗ %s\n' "$m"
        done
        fails=$((fails + 1))
    fi
done < <(jq -c '
    .scenarios
    | to_entries
    | sort_by(.key | split(".") | map(tonumber? // 0))
    | .[]
    | .key as $id
    | .value.title as $title
    | (.value.assertions // [])[]
    | . + {id: $id, title: $title}
' "$TMPDIR/scenarios.json")

echo
echo "────────────────────────────────────"
if [[ "$fails" -gt 0 ]]; then
    printf 'FAILED: %d/%d assertion(s) mismatched\n' "$fails" "$total"
    exit 1
fi
printf 'PASSED: %d/%d assertions match\n' "$total" "$total"
