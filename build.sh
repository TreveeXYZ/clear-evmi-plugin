#!/usr/bin/env bash
# Build the exporter plugin with the SAME Go toolchain as the EVMI server.
#
# A Go plugin is only loadable by a host built with the identical Go release AND
# identical versions of every shared dependency — otherwise plugin.Open fails with
# "plugin was built with a different version of package ...".
#
# GO_VERSION must match the `go` directive of github.com/evmi-cloud/go-evm-indexer
# (check: go list -m -f '{{.GoVersion}}' github.com/evmi-cloud/go-evm-indexer).
# GOTOOLCHAIN pins the exact release; Go downloads it on demand.
set -euo pipefail

GO_VERSION="${GO_VERSION:-go1.24.9}"
OUT="${OUT:-clear-defi.so}"

cd "$(dirname "$0")"
export GOTOOLCHAIN="$GO_VERSION"
export CGO_ENABLED=1

go version
go build -buildmode=plugin -o "$OUT" .
go version -m "$OUT" | head -1
