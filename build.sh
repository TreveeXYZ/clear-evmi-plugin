#!/usr/bin/env bash
# Build clear-defi.so so that the EVMI server can actually plugin.Open() it.
#
# WHY THIS IS NOT JUST `go build -buildmode=plugin`
# -------------------------------------------------
# plugin.Open compares, for every package the host and the plugin share, an 8-byte
# pkghash. Any difference => "plugin was built with a different version of package X".
# That hash depends on THREE things, and all three must match the server binary:
#
#   1. the Go release            — go.mod's `go` directive is only a MINIMUM; the
#                                  published image is built with a later patch release
#   2. every shared dep version  — go.mod must resolve exactly what the server linked
#   3. the ABSOLUTE PATH the indexer source was compiled from
#
# (3) is the non-obvious one: the indexer is built WITHOUT -trimpath, from WORKDIR
# /app (see its Dockerfile), so "/app/pkg/exporter/..." is baked into the hash. A
# plugin resolving the indexer from the module cache compiles the very same source at
# /go/pkg/mod/github.com/evmi-cloud/go-evm-indexer@v.../pkg/exporter and gets a
# DIFFERENT hash. Verified: identical source at two paths => two different hashes;
# same path via `replace` => identical hash, even though one side is the main module
# and the other a dependency. -trimpath does NOT fix it (a trimpath'd host and a
# trimpath'd plugin still disagree) — matching the path is what works.
#
# So this builds in a container: check out the indexer at the exact commit the server
# image was built from, put it at $BUILD_PATH (/app), point the plugin's go.mod there
# with a `replace`, build. The replace is injected into a throwaway copy, so the
# committed go.mod stays clean and `go test ./...` keeps working normally.
#
# Then VERIFY against the real server binary by diffing every shared pkghash, so a
# mismatch fails here instead of at exporter start.
set -euo pipefail

EVMI_IMAGE="${EVMI_IMAGE:-evmicloud/go-evm-indexer:latest}"
INDEXER_REPO="${INDEXER_REPO:-https://github.com/evmi-cloud/go-evm-indexer.git}"
INDEXER_REV="${INDEXER_REV:-}"   # default: the image's org.opencontainers.image.revision label
GO_VERSION="${GO_VERSION:-}"     # default: the Go release stamped in the server binary
BUILD_PATH="${BUILD_PATH:-/app}" # the indexer Dockerfile's builder WORKDIR
OUT="${OUT:-clear-defi.so}"

cd "$(dirname "$0")"
SRC="$PWD"

command -v docker >/dev/null || { echo "docker is required (this build must run in a Linux container)"; exit 1; }
docker image inspect "$EVMI_IMAGE" >/dev/null 2>&1 || docker pull "$EVMI_IMAGE"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- pull the server binary out of the image; it is the source of truth ----------
cid="$(docker create "$EVMI_IMAGE")"
docker cp "$cid":/evm-indexer "$WORK/evm-indexer" >/dev/null
docker rm -f "$cid" >/dev/null

if [ -z "$GO_VERSION" ]; then
  GO_VERSION="$(docker run --rm -v "$WORK":/w --entrypoint sh "$EVMI_IMAGE" \
    -c 'go version -m /w/evm-indexer | head -1' | awk '{print $2}')"
fi
if [ -z "$INDEXER_REV" ]; then
  INDEXER_REV="$(docker image inspect "$EVMI_IMAGE" \
    --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')"
fi
[ -n "$GO_VERSION" ] && [ -n "$INDEXER_REV" ] || { echo "could not determine Go release / indexer revision"; exit 1; }

BUILDER="golang:${GO_VERSION#go}-bookworm"
echo "image       : $EVMI_IMAGE"
echo "go release  : $GO_VERSION  (builder: $BUILDER)"
echo "indexer rev : $INDEXER_REV"
echo "build path  : $BUILD_PATH"

# --- throwaway copy of the plugin source, so go.mod is not mutated in git --------
cp -r "$SRC" "$WORK/plugin"
rm -rf "$WORK/plugin/.git" "$WORK/plugin"/*.so

docker run --rm -v "$WORK":/w -w /w/plugin "$BUILDER" bash -euc "
  export CGO_ENABLED=1 GOTOOLCHAIN=local
  git clone -q '$INDEXER_REPO' '$BUILD_PATH'
  git -C '$BUILD_PATH' checkout -q '$INDEXER_REV'
  go mod edit -replace github.com/evmi-cloud/go-evm-indexer='$BUILD_PATH'
  go mod tidy >/dev/null
  go build -buildmode=plugin -o /w/out.so .
"

# --- verify against the real server binary; refuses to ship a .so that won't load ---
cp "$SRC/verify-plugin.sh" "$WORK/verify-plugin.sh"
docker run --rm -v "$WORK":/w "$BUILDER" bash /w/verify-plugin.sh /w/evm-indexer /w/out.so

cp "$WORK/out.so" "$SRC/$OUT"
echo "wrote $OUT"
