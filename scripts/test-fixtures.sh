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

# A prebuilt binary can be supplied via GH_AUDIT_BIN (e.g. Bazel's
# bazel-bin/gh-audit in CI); only then is the Go toolchain unnecessary.
required_bins="jq duckdb curl"
if [[ -z "${GH_AUDIT_BIN:-}" ]]; then
    required_bins="go $required_bins"
fi
for bin in $required_bins; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        echo "error: required binary '$bin' not found in PATH" >&2
        exit 2
    fi
done

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# write_config DB CONFIGFILE [SIGNING_POLICY] [EXEMPT_ID]
# The §1 signing scenarios (6.x) need an exempt account. Every commit in
# THIS repo was authored by stefanpenner (id 1377), so exempting 1377
# globally would waive every fixture — which is exactly the point of 6.x
# but wrong for the 1.x–5.x scenarios. So the main validation runs with
# NO exemptions, and the signing section below runs dedicated syncs that
# exempt 1377.
write_config() { # $1=db  $2=file  $3=signing_policy(optional)  $4=exempt_id(none)
    cat > "$2" <<EOF
database: $1
orgs:
  - name: ${FIXTURES_REPO%%/*}
    repos: [${FIXTURES_REPO#*/}]
tokens:
  - kind: pat
    env: GITHUB_TOKEN
    scopes:
      - org: ${FIXTURES_REPO%%/*}
        repos: [${FIXTURES_REPO#*/}]
EOF
    if [[ -n "${3:-}" ]]; then
        printf 'audit_rules:\n  signing_policy: %s\n' "$3" >> "$2"
    fi
    if [[ -n "${4:-}" ]]; then
        printf 'exemptions:\n  authors:\n    - login: exempt\n      id: %s\n' "$4" >> "$2"
    fi
}
write_config "$TMPDIR/audit.db" "$TMPDIR/config.yaml"

if [[ -n "${GH_AUDIT_BIN:-}" ]]; then
    echo "==> Using prebuilt gh-audit: $GH_AUDIT_BIN"
else
    echo "==> Building gh-audit"
    GH_AUDIT_BIN="$TMPDIR/gh-audit"
    (cd "$REPO_ROOT" && go build -o "$GH_AUDIT_BIN" .)
fi

echo "==> Syncing $FIXTURES_REPO"
"$GH_AUDIT_BIN" --config "$TMPDIR/config.yaml" sync \
    --repo "$FIXTURES_REPO" \
    --db "$TMPDIR/audit.db" \
    --telemetry-output=- \
    >"$TMPDIR/sync.log" 2>&1 || {
        echo "sync failed; tail of log:" >&2
        tail -n 40 "$TMPDIR/sync.log" >&2
        exit 1
    }

# Re-evaluate everything from the DB before asserting. This validates the
# offline bundle path (the one that once dropped reviewer_id and rejected
# every approval) AND is required for cross-PR facts that only fully
# materialize after the whole sync has landed — e.g. scenario 5.4's
# pr_count=2, whose second commit→PR link is written during the second
# PR's enrichment, after the commit itself was audited.
echo "==> Re-evaluating commits from DB (bundle path)"
"$GH_AUDIT_BIN" --config "$TMPDIR/config.yaml" re-evaluate-commits \
    --db "$TMPDIR/audit.db" \
    >"$TMPDIR/reaudit.log" 2>&1 || {
        echo "re-evaluate-commits failed; tail of log:" >&2
        tail -n 40 "$TMPDIR/reaudit.log" >&2
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
    | select(.key | startswith("6.") | not)   # series 6 = §1 signing, checked separately below
    | .key as $id
    | .value.title as $title
    | (.value.assertions // [])[]
    | . + {id: $id, title: $title}
' "$TMPDIR/scenarios.json")

# ── 6.x: §1 signing policy ──
# Runs its own syncs (not the offline re-evaluate) because these commits'
# diff stats are only lazily fetched when the audit would otherwise flag
# them — the offline bundle path would treat 6.2's unfetched 0/0 stats as
# an "empty commit" and waive it. A real sync fetches the stats.
#
# Exempts account 1377 (stefanpenner). That waives EVERY fixture (all are
# 1377-authored), so this uses throwaway DBs and only asserts on 6.x.
SHA_61="1b9b3917689f370c6adf8defcc722349bf7fdfd0"   # verified signer, exempt id
SHA_62="ae9e372beb9111f5b9fdef4cf1a4a956382f453e"   # unsigned, forged exempt author

SDB="$TMPDIR/sign.db"
sig_get() { duckdb -noheader -csv "$SDB" \
    "SELECT COALESCE(is_compliant,false)||','||COALESCE(is_exempt_author,false)||','||COALESCE(list_contains(annotations,'trust:forgeable-exemption'),false) FROM audit_results WHERE sha='$1';" | tr -d '"'; }
assert_sig() { # id want got desc
    total=$((total + 1))
    if [[ "$3" == "$2" ]]; then
        printf '  OK    %s — %s\n        (compliant,exempt,forgeable)=%s\n' "$1" "$4" "$3"
    else
        printf '  FAIL  %s — %s\n        want (compliant,exempt,forgeable)=%s got=%s\n' "$1" "$4" "$2" "$3"
        fails=$((fails + 1))
    fi
}

# One sync under REQUIRED (fetches 6.2's diff stats, since it's non-compliant
# there and the audit resolves them lazily), then an offline re-evaluate under
# OPTIONAL. Because the stats are now in the DB, the optional pass no longer
# mistakes 6.2's 0/0 for an empty commit. Two policies, one API sync.
write_config "$SDB" "$SDB.req.yaml" required 1377
write_config "$SDB" "$SDB.opt.yaml" optional 1377

printf '\n  ── 6.x: §1 signing policy (required — lock-down) ──\n\n'
"$GH_AUDIT_BIN" --config "$SDB.req.yaml" sync --repo "$FIXTURES_REPO" --db "$SDB" \
    --telemetry-output=- >"$SDB.req.log" 2>&1 || { echo "signing sync failed; tail:" >&2; tail -n 40 "$SDB.req.log" >&2; exit 1; }
assert_sig "6.1" "true,true,false"   "$(sig_get "$SHA_61")" "verified signer stays exempt"
assert_sig "6.2" "false,false,false" "$(sig_get "$SHA_62")" "unsigned forged author fails closed"

printf '\n  ── 6.x: §1 signing policy (optional — progressive enhancement) ──\n\n'
"$GH_AUDIT_BIN" --config "$SDB.opt.yaml" re-evaluate-commits --db "$SDB" \
    >"$SDB.opt.log" 2>&1 || { echo "signing re-evaluate failed; tail:" >&2; tail -n 40 "$SDB.opt.log" >&2; exit 1; }
assert_sig "6.1" "true,true,false" "$(sig_get "$SHA_61")" "verified signer — sound exemption"
assert_sig "6.2" "true,true,true"  "$(sig_get "$SHA_62")" "unsigned forged author — waived but flagged forgeable"

echo
echo "────────────────────────────────────"
if [[ "$fails" -gt 0 ]]; then
    printf 'FAILED: %d/%d assertion(s) mismatched\n' "$fails" "$total"
    exit 1
fi
printf 'PASSED: %d/%d assertions match\n' "$total" "$total"
