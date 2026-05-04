#!/usr/bin/env bash
# check-gojq-purity.sh — pin the safeCopyJSON discipline at every gojq
# call site outside the vendored gojq/ tree.
#
# Per /tmp/snowplow-runs/gojq-purity-audit-2026-05-03.md the audit
# verdict is NEEDS-AUDIT-AT-CALL-SITE: gojq's `deleteEmpty` mutates input
# trees in-place. Snowplow shields informer storage from this mutation
# by wrapping every gojq input with safeCopyJSON. This script enforces
# that invariant: every direct gojq.Eval / gojq.Run / gojq.Code.Run call
# site (excluding gojq/ internals) MUST either:
#
#   1. carry a `// gojq-purity-required` marker comment within 5 lines
#      above the call, OR
#   2. be preceded by a `safeCopyJSON(` invocation within 5 lines above.
#
# jqutil.Eval is the indirect wrapper used by snowplow; the marker is
# applied at the helper-function level (jsonHandlerCompute /
# jsonHandlerDirectCompute / etc.). Direct gojq.* in non-test files
# outside gojq/ should be rare and audited.
#
# Exit codes:
#   0  = all sites covered
#   1  = at least one bare site (printed to stderr)
#
# Usage:
#   ./scripts/check-gojq-purity.sh         # check
#   ./scripts/check-gojq-purity.sh --list  # list all relevant sites

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Find direct gojq.Eval / gojq.Run / gojq.Code.Run / Code.Run calls in
# package code, excluding the vendored gojq/ tree itself, _test.go files
# (test code is allowed to call gojq directly), and the legacy stub
# normalizeForJQ in handler.go.
HITS_FILE="$(mktemp)"
trap 'rm -f "$HITS_FILE"' EXIT
grep -rn -E 'gojq\.(Eval|Run|Code)\b|jqutil\.Eval\(' \
    --include='*.go' \
    --exclude-dir=gojq \
    --exclude-dir=.claude \
    --exclude-dir=.git \
    --exclude='*_test.go' \
    "$ROOT" 2>/dev/null > "$HITS_FILE" || true

if [[ "${1:-}" == "--list" ]]; then
  cat "$HITS_FILE"
  exit 0
fi

bad=0
while IFS= read -r hit; do
  [[ -z "$hit" ]] && continue
  file="${hit%%:*}"
  rest="${hit#*:}"
  line="${rest%%:*}"

  # Look back up to 5 lines for safeCopyJSON or the marker comment.
  start=$((line - 5))
  if (( start < 1 )); then start=1; end_=$((line - 1)); else end_=$((line - 1)); fi
  if (( end_ < start )); then end_=$start; fi

  ctx="$(sed -n "${start},${line}p" "$file" 2>/dev/null || true)"

  if grep -q 'gojq-purity-required' <<<"$ctx"; then
    continue
  fi
  if grep -q 'safeCopyJSON(' <<<"$ctx"; then
    continue
  fi

  echo "MISSING gojq-purity discipline at $hit" >&2
  echo "    context (lines ${start}..${line}):" >&2
  sed -n "${start},${line}p" "$file" | sed 's/^/      | /' >&2
  bad=$((bad + 1))
done < "$HITS_FILE"

if (( bad > 0 )); then
  echo "" >&2
  echo "FAIL: $bad gojq call site(s) missing safeCopyJSON or // gojq-purity-required marker" >&2
  echo "      see /tmp/snowplow-runs/gojq-purity-audit-2026-05-03.md" >&2
  exit 1
fi

echo "ok: all gojq call sites covered by safeCopyJSON discipline or marker"
