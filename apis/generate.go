//go:build generate
// +build generate

// generate.go drives CRD + deepcopy codegen — Stage 1 of the CRD
// pipeline (Go type -> crds/).
//
// controller-gen VERSION PIN: the `go run sigs.k8s.io/controller-tools/
// cmd/controller-gen` invocation below resolves controller-gen to the
// EXACT sigs.k8s.io/controller-tools version required in go.mod (the
// blank import here keeps that module a direct, version-pinned
// dependency that `go mod tidy` cannot prune). Bumping the CRD codegen
// tool is therefore a deliberate go.mod edit + `bash scripts/gen.sh`
// regeneration — never an implicit drift. `scripts/gen.sh --check`
// (run by the release-pullrequest CI `codegen` job) fails the build if
// crds/ ever diverges from what this pinned tool produces.

// Remove existing CRDs
//go:generate rm -rf ../crds

// Generate deepcopy methodsets and CRD manifests
//go:generate go run -tags generate sigs.k8s.io/controller-tools/cmd/controller-gen object:headerFile=../hack/boilerplate.go.txt paths=./... crd:crdVersions=v1 output:artifacts:config=../crds

package apis

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen" //nolint:typecheck
)
