#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Snowplow performance benchmark — cache ON vs cache OFF
# Compatible with bash 3.x (macOS default)
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

SNOWPLOW="${SNOWPLOW_URL:-http://localhost:30081}"
AUTHN="${AUTHN_URL:-http://localhost:30082}"
USERNAME="${USERNAME:-admin}"
PASSWORD="${PASSWORD:-agPEjgMStMYH}"
ITERATIONS="${ITERATIONS:-30}"
WARMUP="${WARMUP:-5}"

# label | path
ENDPOINTS=(
  "Page dashboard         |/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages&name=dashboard-page&namespace=krateo-system"
  "Page blueprints        |/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages&name=blueprints-page&namespace=krateo-system"
  "Page compositions      |/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages&name=compositions-page&namespace=krateo-system"
  "NavMenu                |/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=navmenus&name=sidebar-nav-menu&namespace=krateo-system"
  "RESTAction all-routes  |/call?apiVersion=templates.krateo.io/v1&resource=restactions&name=all-routes&namespace=krateo-system"
  "RESTAction bp-list     |/call?apiVersion=templates.krateo.io/v1&resource=restactions&name=blueprints-list&namespace=krateo-system"
  "ConfigMaps list        |/call?apiVersion=v1&resource=configmaps&namespace=krateo-system"
)

BASIC=$(printf '%s:%s' "$USERNAME" "$PASSWORD" | base64)

# Results stored as parallel plain arrays (bash 3 compatible)
LABELS=()
C_P50=(); C_P90=(); C_P99=(); C_MEAN=()
N_P50=(); N_P90=(); N_P99=(); N_MEAN=()

get_token() {
  curl -sf "$AUTHN/basic/login" \
    -H "Authorization: Basic $BASIC" \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"
}

# Emit one integer (ms) per request
benchmark_endpoint() {
  local token="$1" path="$2" iters="$3"
  for ((i=0; i<iters; i++)); do
    curl -sf -o /dev/null \
      -w "%{time_total}\n" \
      -H "Authorization: Bearer $token" \
      "${SNOWPLOW}${path}"
  done | awk '{printf "%d\n", $1 * 1000}'
}

compute_stats() {
  python3 - <<'PYEOF'
import sys, statistics
vals = [int(l) for l in sys.stdin if l.strip()]
if not vals:
    print("0 0 0 0 0"); exit()
s = sorted(vals)
n = len(s)
def pct(p):
    return s[max(0, int(round(p/100*n)) - 1)]
print(pct(50), pct(90), pct(99), pct(100), round(statistics.mean(vals)))
PYEOF
}

wait_for_snowplow() {
  echo "  ⏳ waiting for snowplow..."
  for i in $(seq 1 30); do
    if curl -sf "$SNOWPLOW/health" -o /dev/null 2>/dev/null; then
      echo "  ✓ snowplow ready"; return 0
    fi
    sleep 2
  done
  echo "  ✗ snowplow not ready"; exit 1
}

run_benchmark() {
  local mode="$1"
  local token
  token=$(get_token)
  echo "  ✓ JWT acquired"

  local idx=0
  for entry in "${ENDPOINTS[@]}"; do
    local label="${entry%%|*}"
    local path="${entry##*|}"
    [[ "$mode" == "cache" ]] && LABELS+=("$label")

    printf "  %-26s ... " "$label"
    benchmark_endpoint "$token" "$path" "$WARMUP" > /dev/null  # warmup

    read -r p50 p90 p99 mx mean \
      < <(benchmark_endpoint "$token" "$path" "$ITERATIONS" | compute_stats)

    if [[ "$mode" == "cache" ]]; then
      C_P50+=("$p50"); C_P90+=("$p90"); C_P99+=("$p99"); C_MEAN+=("$mean")
    else
      N_P50+=("$p50"); N_P90+=("$p90"); N_P99+=("$p99"); N_MEAN+=("$mean")
    fi

    echo "mean=${mean}ms  p50=${p50}ms  p90=${p90}ms  p99=${p99}ms"
    (( idx++ ))
  done
}

print_comparison() {
  local sep
  sep=$(printf '%0.s─' {1..98})

  echo ""
  printf '%0.s━' {1..98}; echo ""
  echo "  RESULTS  (${ITERATIONS} iterations · ${WARMUP} warmup · all times in ms)"
  printf '%0.s━' {1..98}; echo ""
  printf "  %-26s  %7s %7s %7s  │  %7s %7s %7s  │  %8s\n" \
    "Endpoint" "c·p50" "c·p90" "c·p99" "n·p50" "n·p90" "n·p99" "speedup"
  echo "  $sep"

  local c_total=0 n_total=0 count="${#LABELS[@]}"
  for i in $(seq 0 $(( count - 1 ))); do
    local cp50="${C_P50[$i]}" cp90="${C_P90[$i]}" cp99="${C_P99[$i]}"
    local np50="${N_P50[$i]}" np90="${N_P90[$i]}" np99="${N_P99[$i]}"
    local speedup
    if [[ $cp50 -gt 0 ]]; then
      speedup=$(awk "BEGIN{printf \"%.1fx\", $np50/$cp50}")
    else
      speedup="  N/A"
    fi
    printf "  %-26s  %6dms %6dms %6dms  │  %6dms %6dms %6dms  │  %8s\n" \
      "${LABELS[$i]}" "$cp50" "$cp90" "$cp99" "$np50" "$np90" "$np99" "$speedup"
    c_total=$(( c_total + ${C_MEAN[$i]} ))
    n_total=$(( n_total + ${N_MEAN[$i]} ))
  done

  local c_avg=$(( c_total / count ))
  local n_avg=$(( n_total / count ))
  local overall
  overall=$(awk "BEGIN{printf \"%.1fx\", $n_avg/$c_avg}")
  echo "  $sep"
  printf "  %-26s  %7s %7s %7s  │  %7s %7s %7s  │  %8s\n" \
    "Average (mean of means)" "${c_avg}ms" "" "" "${n_avg}ms" "" "" "$overall"
  printf '%0.s━' {1..98}; echo ""
  echo ""
}

# ── Main ─────────────────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════════"
echo "  Snowplow Performance Benchmark"
echo "  Snowplow  : $SNOWPLOW"
echo "  Iterations: $ITERATIONS  Warmup: $WARMUP"
echo "════════════════════════════════════════════════════════"

echo ""
echo "── Phase 1/3: Benchmark — Cache ENABLED ────────────────"
run_benchmark "cache"

echo ""
echo "── Phase 2/3: Removing Redis, restarting snowplow ──────"
kubectl patch deployment snowplow -n krateo-system --type=json \
  -p='[{"op":"remove","path":"/spec/template/spec/initContainers"}]'
kubectl rollout status deployment/snowplow -n krateo-system --timeout=120s
wait_for_snowplow
sleep 3

echo ""
echo "── Phase 2/3: Benchmark — Cache DISABLED ───────────────"
run_benchmark "nocache"

echo ""
echo "── Phase 3/3: Restoring Redis sidecar ──────────────────"
kubectl patch deployment snowplow -n krateo-system --type=strategic \
  -p='{"spec":{"template":{"spec":{"initContainers":[{"name":"redis","image":"redis:7-alpine","restartPolicy":"Always","ports":[{"containerPort":6379}],"readinessProbe":{"exec":{"command":["redis-cli","ping"]},"initialDelaySeconds":2,"periodSeconds":3},"resources":{"requests":{"cpu":"50m","memory":"64Mi"},"limits":{"cpu":"200m","memory":"128Mi"}}}]}}}}'
echo "  Redis sidecar patch applied (background rollout)"

print_comparison
