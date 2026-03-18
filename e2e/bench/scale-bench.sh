#!/usr/bin/env bash
set -euo pipefail

TOTAL_NS=100
COMPS_PER_NS=10

echo "=== Creating $TOTAL_NS namespaces with $COMPS_PER_NS compositions each ==="
echo "    Total compositions: $((TOTAL_NS * COMPS_PER_NS))"

for ns_i in $(seq 1 $TOTAL_NS); do
  NS=$(printf "bench-ns-%02d" "$ns_i")

  # Create namespace if it doesn't exist
  kubectl get ns "$NS" &>/dev/null || kubectl create ns "$NS"

  for comp_i in $(seq 1 $COMPS_PER_NS); do
    APP=$(printf "bench-app-%02d-%02d" "$ns_i" "$comp_i")

    # Skip if already exists
    if kubectl get githubscaffoldingwithcompositionpages.composition.krateo.io "$APP" -n "$NS" &>/dev/null 2>&1; then
      continue
    fi

    cat <<EOF | kubectl apply -f - &>/dev/null
apiVersion: composition.krateo.io/v1-2-2
kind: GithubScaffoldingWithCompositionPage
metadata:
  name: ${APP}
  namespace: ${NS}
spec:
  app:
    service:
      port: 31180
      type: NodePort
  argocd:
    application:
      destination:
        namespace: ${APP}
        server: https://kubernetes.default.svc
      project: default
      source:
        path: chart/
      syncEnabled: false
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
    namespace: krateo-system
  git:
    fromRepo:
      branch: main
      credentials:
        authMethod: generic
        secretRef:
          key: token
          name: github-repo-creds
          namespace: krateo-system
      name: github-scaffolding-with-composition-page
      org: krateoplatformops-blueprints
      path: skeleton/
      scmUrl: https://github.com
    insecure: true
    toRepo:
      branch: main
      configurationRef:
        name: repo-config
        namespace: demo-system
      credentials:
        authMethod: generic
        secretRef:
          key: token
          name: github-repo-creds
          namespace: krateo-system
      deletionPolicy: Delete
      initialize: true
      name: ${APP}
      org: krateoplatformops-test
      path: /
      private: false
      scmUrl: https://github.com
      verbose: false
    unsupportedCapabilities: true
EOF
  done
  echo "  ns=$NS: $COMPS_PER_NS compositions applied"
done

echo ""
echo "=== Done. Counting total compositions ==="
kubectl get githubscaffoldingwithcompositionpages.composition.krateo.io -A --no-headers 2>/dev/null | wc -l
