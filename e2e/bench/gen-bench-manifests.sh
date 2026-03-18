#!/usr/bin/env bash
set -euo pipefail

TOTAL_NS=100
COMPS_PER_NS=10
OUT="/tmp/bench-manifests.yaml"

> "$OUT"

for ns_i in $(seq 1 $TOTAL_NS); do
  NS=$(printf "bench-ns-%02d" "$ns_i")

  cat >> "$OUT" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${NS}
---
EOF

  for comp_i in $(seq 1 $COMPS_PER_NS); do
    APP=$(printf "bench-app-%02d-%02d" "$ns_i" "$comp_i")

    cat >> "$OUT" <<EOF
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
---
EOF
  done
done

echo "Generated $(grep -c '^kind: GithubScaffoldingWithCompositionPage' "$OUT") compositions in $OUT"
echo "File size: $(du -h "$OUT" | cut -f1)"
