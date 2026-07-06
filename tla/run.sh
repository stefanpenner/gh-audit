#!/usr/bin/env bash
# Model-check the gh-audit soundness specs (Architecture.md S1-S8).
#
#   ./tla/run.sh            check every module, assert expected outcomes
#   ./tla/run.sh Approval   check one module (all its configs)
#   ./tla/run.sh list       list modules and configs
#
# Convention, by config suffix:
#   *_green.cfg  shipped rule      -> Sound MUST hold
#   *_red.cfg    retired/naive rule -> Sound MUST be violated (attack found)
#   *_amber.cfg  shipped-with-known-tradeoff -> Sound violated, DOCUMENTED
#   *_bait.cfg   vacuity check     -> Bait (~Compliant) MUST be violated,
#                proving a compliant state is reachable at green bounds
#
# Each line also prints the distinct-state count, so the bounds every
# verdict rests on are recorded in the run output.
#
# Fetches tla2tools.jar on first run. Needs a JRE (>= 11); set JAVA to override.
set -uo pipefail
cd "$(dirname "$0")"

TOOLS_DIR=".tools"
JAR="$TOOLS_DIR/tla2tools.jar"
JAR_URL="https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar"

find_java() {
    if [[ -n "${JAVA:-}" ]]; then echo "$JAVA"; return; fi
    for c in /opt/homebrew/opt/openjdk/bin/java java; do
        "$c" -version >/dev/null 2>&1 && { echo "$c"; return; }
    done
    echo "error: no usable java; install a JRE or set JAVA" >&2; exit 1
}
JAVA_BIN="$(find_java)"

if [[ ! -f "$JAR" ]]; then
    mkdir -p "$TOOLS_DIR"; echo "fetching tla2tools.jar ..."
    curl -sL -o "$JAR" "$JAR_URL"
fi

check() { # module cfg -> prints outcome token
    "$JAVA_BIN" -XX:+UseParallelGC -cp "$JAR" tlc2.TLC -deadlock \
        -config "$2" "$1.tla" 2>&1
}

classify() { # raw-output -> HOLDS | <Invariant>-VIOLATED | ERROR
    local inv
    if grep -q "No error has been found" <<<"$1"; then echo HOLDS; return; fi
    inv=$(grep -o "Invariant [A-Za-z0-9_]* is violated" <<<"$1" | head -1 | awk '{print $2}')
    if [[ -n "$inv" ]]; then echo "${inv}-VIOLATED"; else echo ERROR; fi
}

states_of() { # raw-output -> distinct-state count (or ?)
    local n
    n=$(grep -oE '[0-9]+ distinct states' <<<"$1" | head -1 | cut -d' ' -f1)
    echo "${n:-?}"
}

fail=0
modules=$(ls *_green.cfg 2>/dev/null | sed 's/_green.cfg//')
[[ $# -gt 0 && "$1" == "list" ]] && { echo "$modules"; exit 0; }
[[ $# -gt 0 ]] && modules="$1"

for m in $modules; do
    echo "== $m =="
    for cfg in ${m}_green.cfg ${m}_red.cfg ${m}_amber.cfg ${m}_bait.cfg; do
        [[ -f "$cfg" ]] || continue
        suffix="${cfg#${m}_}"; suffix="${suffix%.cfg}"
        out=$(check "$m" "$cfg")
        got=$(classify "$out")
        case "$suffix" in
            green) want=HOLDS ;;
            red|amber) want=Sound-VIOLATED ;;
            bait) want=Bait-VIOLATED ;;
        esac
        if [[ "$got" == "$want" ]]; then
            mark="ok"
        else
            mark="FAIL (wanted $want)"; fail=1
        fi
        printf "  %-6s %-16s %8s states  %s\n" "$suffix" "$got" "$(states_of "$out")" "$mark"
    done
done

echo
if [[ $fail -eq 0 ]]; then echo "all specs behaved as expected"; else echo "SPEC MISMATCH — see FAIL above"; fi
exit $fail
