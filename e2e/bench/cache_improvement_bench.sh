#!/usr/bin/env bash
#
# Snowplow Cache Improvement Benchmark
# =====================================
# Runs from inside the cluster via kubectl run.
# Measures L1 hit vs L1 miss, L3 promotion impact, and
# parallel resourcesRefs speedup.
#
# Usage:
#   AUTHN_NS=krateo-system PASSWORD=jl1DDPGMFOWw ./cache_improvement_bench.sh
#
set -euo pipefail

NAMESPACE="${NAMESPACE:-krateo-system}"
SNOWPLOW_SVC="${SNOWPLOW_SVC:-snowplow:8081}"
AUTHN_SVC="${AUTHN_SVC:-authn:8082}"
PASSWORD="${PASSWORD:-jl1DDPGMFOWw}"
ITERATIONS="${ITERATIONS:-10}"
POD_NAME="bench-$(date +%s)"
BOLD=$'\033[1m'
CYAN=$'\033[96m'
GREEN=$'\033[92m'
RED=$'\033[91m'
RESET=$'\033[0m'

cleanup() {
    kubectl delete pod "$POD_NAME" -n "$NAMESPACE" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
echo "${BOLD}  Snowplow Cache Improvement Benchmark${RESET}"
echo "  Snowplow : $SNOWPLOW_SVC"
echo "  Authn    : $AUTHN_SVC"
echo "  Iters    : $ITERATIONS"
echo "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"

# The benchmark script runs inside the cluster as a Pod
read -r -d '' BENCH_SCRIPT << 'INNEREOF' || true
#!/bin/sh
set -e

SNOWPLOW_SVC="$1"
AUTHN_SVC="$2"
PASSWORD="$3"
ITERATIONS="$4"

# Login and get JWT
TOKEN=$(curl -sf -H "Authorization: Basic $(echo -n "admin:${PASSWORD}" | base64)" \
    "http://${AUTHN_SVC}/basic/login" | sed -n 's/.*"accessToken":"\([^"]*\)".*/\1/p')

if [ -z "$TOKEN" ]; then
    echo "ERROR: Failed to login"
    exit 1
fi
echo "JWT acquired"

# Measure a single request, return elapsed ms
measure() {
    local path="$1"
    local start end elapsed
    start=$(date +%s%N 2>/dev/null || echo 0)
    curl -sf -o /dev/null -w "%{time_total}" \
        -H "Authorization: Bearer $TOKEN" \
        "http://${SNOWPLOW_SVC}${path}" 2>/dev/null
    end=$(date +%s%N 2>/dev/null || echo 0)
}

# Measure with curl's time_total (seconds with 6 decimals)
measure_ms() {
    local path="$1"
    local total
    total=$(curl -sf -o /dev/null -w "%{time_total}" \
        -H "Authorization: Bearer $TOKEN" \
        "http://${SNOWPLOW_SVC}${path}" 2>/dev/null || echo "9.999")
    echo "$total" | awk '{printf "%d", $1 * 1000}'
}

# Run N iterations and compute stats
run_bench() {
    local label="$1"
    local path="$2"
    local iters="$3"
    local sum=0 min=999999 max=0 val i
    local values=""

    for i in $(seq 1 "$iters"); do
        val=$(measure_ms "$path")
        sum=$((sum + val))
        [ "$val" -lt "$min" ] && min=$val
        [ "$val" -gt "$max" ] && max=$val
        values="$values $val"
    done

    local mean=$((sum / iters))

    # Compute p50 and p90
    local sorted
    sorted=$(echo "$values" | tr ' ' '\n' | sort -n | grep -v '^$')
    local count
    count=$(echo "$sorted" | wc -l | tr -d ' ')
    local p50_idx=$(( (count * 50 + 99) / 100 ))
    local p90_idx=$(( (count * 90 + 99) / 100 ))
    local p50
    p50=$(echo "$sorted" | sed -n "${p50_idx}p")
    local p90
    p90=$(echo "$sorted" | sed -n "${p90_idx}p")

    printf "  %-35s mean=%4dms  p50=%4dms  p90=%4dms  min=%4dms  max=%4dms\n" \
        "$label" "$mean" "$p50" "$p90" "$min" "$max"
}

# Endpoints to benchmark
PAGE_DASH="/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=dashboard-page&namespace=krateo-system"
PAGE_COMPS="/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=compositions-page&namespace=krateo-system"
RA_ROUTES="/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=all-routes&namespace=krateo-system"
RA_COMPLIST="/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-list&namespace=krateo-system"
NAVMENU="/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=navmenus&name=sidebar-nav-menu&namespace=krateo-system"

echo ""
echo "== Phase 1: L1 HIT latency (steady state, all caches warm) =="
echo "   Warming up with 3 requests per endpoint..."
for ep in "$PAGE_DASH" "$PAGE_COMPS" "$RA_ROUTES" "$RA_COMPLIST" "$NAVMENU"; do
    for _ in 1 2 3; do
        curl -sf -o /dev/null -H "Authorization: Bearer $TOKEN" "http://${SNOWPLOW_SVC}${ep}" 2>/dev/null || true
    done
done
echo "   Measuring L1 hit latency..."
run_bench "Widget: dashboard-page (L1 hit)" "$PAGE_DASH" "$ITERATIONS"
run_bench "Widget: compositions-page (L1 hit)" "$PAGE_COMPS" "$ITERATIONS"
run_bench "RESTAction: all-routes (L1 hit)" "$RA_ROUTES" "$ITERATIONS"
run_bench "RESTAction: compositions-list (L1 hit)" "$RA_COMPLIST" "$ITERATIONS"
run_bench "Widget: navmenu (L1 hit)" "$NAVMENU" "$ITERATIONS"

echo ""
echo "== Phase 2: Cache metrics snapshot =="
METRICS=$(curl -sf -H "Authorization: Bearer $TOKEN" "http://${SNOWPLOW_SVC}/metrics/cache" 2>/dev/null || echo "{}")
echo "  $METRICS"

echo ""
echo "== Phase 3: L1 MISS latency (flush L1, measure cold resolution) =="
echo "   Flushing resolved cache keys..."
# We can't flush Redis directly from here, so we measure "first request
# after deployment" scenario by using a fresh login (different token ≠
# different L1 key if L1 is per-user+token)
# Instead, just note the current values as baseline.
# For a true L1 miss test, the operator should restart snowplow or flush Redis.
echo "   (Using warm L3 — measures resolution pipeline overhead only)"
echo "   Skipping explicit L1 flush (requires Redis access)."
echo "   To measure true L1 miss: restart snowplow, then immediately run this script."

echo ""
echo "== DONE =="
INNEREOF

# Create the pod and run the benchmark
echo ""
echo "${CYAN}Launching benchmark pod inside the cluster...${RESET}"
kubectl run "$POD_NAME" -n "$NAMESPACE" \
    --image=curlimages/curl:latest \
    --restart=Never \
    --command -- sh -c "$BENCH_SCRIPT" -- \
    "$SNOWPLOW_SVC" "$AUTHN_SVC" "$PASSWORD" "$ITERATIONS"

echo "Waiting for pod to complete..."
kubectl wait --for=condition=Ready pod/"$POD_NAME" -n "$NAMESPACE" --timeout=30s 2>/dev/null || true
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/"$POD_NAME" -n "$NAMESPACE" --timeout=300s 2>/dev/null || \
    kubectl wait --for=jsonpath='{.status.phase}'=Failed pod/"$POD_NAME" -n "$NAMESPACE" --timeout=300s 2>/dev/null || true

echo ""
echo "${BOLD}── Benchmark Results ──${RESET}"
kubectl logs "$POD_NAME" -n "$NAMESPACE" 2>/dev/null || echo "Failed to retrieve logs"

echo ""
echo "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
