#!/bin/bash
# Targeted S7 test: deploy 50K, wait for convergence, delete 1, capture logs
# Run after the cluster already has 50K compositions deployed
set -e

SNOWPLOW_URL="${SNOWPLOW_URL:-http://34.135.50.203:8081}"
AUTHN_URL="${AUTHN_URL:-http://34.136.84.51:8082}"
COMP_RES="githubscaffoldingwithcompositionpages"
COMP_GVR="composition.krateo.io"
NS="krateo-system"

echo "=== Targeted S7 Test ==="
echo "Snowplow: $SNOWPLOW_URL"

# Login
echo "Logging in as admin..."
TOKEN=$(curl -s "$AUTHN_URL/basic/login" -H "Authorization: Basic $(echo -n 'admin:zKQAMSGJ3S4S' | base64)" | python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])")
echo "Token acquired"

# Check current count
CLUSTER_COUNT=$(kubectl get "$COMP_RES.$COMP_GVR" --all-namespaces --no-headers 2>/dev/null | wc -l | tr -d ' ')
echo "Cluster compositions: $CLUSTER_COUNT"

# Get API count via piechart
API_RESP=$(curl -s "$SNOWPLOW_URL/call?resource=piecharts&apiVersion=widgets.templates.krateo.io%2Fv1beta1&name=dashboard-compositions-panel-row-piechart&namespace=krateo-system" -H "Authorization: Bearer $TOKEN")
echo "API response length: ${#API_RESP}"

# Wait for L1 to be stable (no pending refreshes)
echo "Waiting 30s for L1 stability..."
sleep 30

# Capture pre-delete logs
echo "Capturing pre-delete logs..."
kubectl logs -n $NS -l app.kubernetes.io/name=snowplow -c snowplow --tail=200 > /tmp/s7_pre_delete.txt 2>&1
echo "Pre-delete: $(wc -l < /tmp/s7_pre_delete.txt) lines"

# Delete one composition
echo "Deleting bench-ns-01/bench-app-01-01..."
kubectl patch "$COMP_RES.$COMP_GVR" bench-app-01-01 -n bench-ns-01 --type=merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null
kubectl delete "$COMP_RES.$COMP_GVR" bench-app-01-01 -n bench-ns-01 --wait=false 2>/dev/null
echo "Delete command sent at $(date +%H:%M:%S)"

# Poll every 5s for 300s, capturing logs at each poll
for i in $(seq 1 60); do
    sleep 5

    # Get API count
    API_RESP=$(curl -s "$SNOWPLOW_URL/call?resource=piecharts&apiVersion=widgets.templates.krateo.io%2Fv1beta1&name=dashboard-compositions-panel-row-piechart&namespace=krateo-system" -H "Authorization: Bearer $TOKEN" 2>/dev/null)

    # Extract readyTrue count from piechart
    API_COUNT=$(echo "$API_RESP" | python3 -c "
import sys,json
try:
    d=json.load(sys.stdin)
    items=d.get('status',{}).get('content',{}).get('readyTrue',0)
    print(items)
except:
    print('error')
" 2>/dev/null)

    CLUSTER_NOW=$(kubectl get "$COMP_RES.$COMP_GVR" --all-namespaces --no-headers 2>/dev/null | wc -l | tr -d ' ')

    echo "Poll $i ($(( i * 5 ))s): api=$API_COUNT cluster=$CLUSTER_NOW"

    if [ "$API_COUNT" = "$CLUSTER_NOW" ] && [ "$CLUSTER_NOW" != "$CLUSTER_COUNT" ]; then
        echo "CONVERGED at $(( i * 5 ))s!"
        break
    fi
done

# Capture post-delete logs IMMEDIATELY
echo "Capturing post-delete logs..."
kubectl logs -n $NS -l app.kubernetes.io/name=snowplow -c snowplow --since=600s > /tmp/s7_post_delete.txt 2>&1
echo "Post-delete: $(wc -l < /tmp/s7_post_delete.txt) lines"

# Analyze logs
echo ""
echo "=== Log Analysis ==="
echo "Dispatch compositions-list:"
grep "dispatch.*compositions-list" /tmp/s7_post_delete.txt | tail -5
echo ""
echo "Refresh compositions-list:"
grep "refreshSingleL1.*compositions-list" /tmp/s7_post_delete.txt | tail -5
echo ""
echo "Cascade from compositions-list:"
grep "cascade.*compositions-list\|cascade.*from.*compositions" /tmp/s7_post_delete.txt | tail -5
echo ""
echo "HOT/WARM/COLD batches:"
grep "hot.*warm.*cold" /tmp/s7_post_delete.txt | tail -5
echo ""
echo "L1 refresh done (composition trigger):"
grep "L1 refresh: done.*composition" /tmp/s7_post_delete.txt | tail -5
echo ""
echo "Full logs saved to /tmp/s7_post_delete.txt"
