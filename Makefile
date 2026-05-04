# Snowplow Makefile — minimal targets for gojq-purity check + standard
# build/test. The gojq-purity-check target is the CI gate that pins the
# safeCopyJSON discipline at every direct gojq call site (audit:
# /tmp/snowplow-runs/gojq-purity-audit-2026-05-03.md).

.PHONY: build test test-race gojq-purity-check ci

build:
	go build ./...

test:
	go test ./...

test-race:
	go test -race ./...

# gojq-purity-check enforces that every direct gojq.Eval / gojq.Run call
# (outside the vendored gojq/ tree) is preceded by safeCopyJSON or
# carries a `// gojq-purity-required` marker comment.
gojq-purity-check:
	@bash scripts/check-gojq-purity.sh

ci: gojq-purity-check build test-race
