# The production EVMI image: the indexer server (built from our pinned fork)
# plus this plugin's source baked in as a local git repository.
#
# Why the runtime image keeps git and the Go toolchain: EVMI's only plugin
# source is `git clone` + `go build` at startup (internal/exporter/loader.go —
# a fresh internal database always builds; the skip-build path needs a prior
# "installed" record). So the image bakes the plugin as a local repo, points
# gitUrl at it (file:///opt/clear-evmi-plugin, empty gitRef = its default
# branch), pre-fills the module and build caches, and lets the first boot do
# the one native build (~seconds, fully offline — no GitHub, no module proxy).
#
# Config IS baked — as a template. deploy/config.prod.json is the committed,
# reviewable config with exactly three secret slots (${EVMI_DB_DSN},
# ${CLEAR_DB_DSN}, ${RPC_URL}); the entrypoint renders it at boot from
# environment variables (Cloud Run secret_env <- Secret Manager) and starts
# the server. Changing anything else in the config is a PR + release.
ARG GO_IMAGE=golang:1.25-bookworm

FROM ${GO_IMAGE} AS server
# Pinned to the same commit go.mod pins the SDK to (TreveeXYZ/go-evm-indexer is
# our backup fork of evmi-cloud/go-evm-indexer; sync it before bumping this).
# 0c9de93 = upstream main with our two merged PRs (bounded SQL pools,
# gitRef by commit id) and nothing else.
ARG EVMI_REPO=https://github.com/TreveeXYZ/go-evm-indexer.git
ARG EVMI_REF=bb7b39a9c49ec3cb209239aaed06ee4507e2d890
RUN git clone --filter=blob:none ${EVMI_REPO} /src \
 && cd /src && git checkout --quiet ${EVMI_REF} \
 && go build -o /evm-indexer ./cmd/evm-indexer

FROM ${GO_IMAGE}
COPY --from=server /evm-indexer /usr/local/bin/evm-indexer

# Bake the plugin as a fresh single-commit repo: independent of how the build
# context was checked out (shallow CI clones included), and the clone EVMI makes
# from it is deterministic.
COPY . /opt/clear-evmi-plugin
RUN cd /opt/clear-evmi-plugin \
 && rm -rf .git \
 && git init --quiet --initial-branch=main \
 && git add --all \
 && git -c user.email=build@clear.invalid -c user.name=build commit --quiet -m "baked at image build" \
 # Warm the caches so the boot-time `go build` needs no network:
 && go mod download \
 && go build -o /dev/null .

# The gcp-pubsub exporter plugin gets the SAME treatment as this plugin:
# source baked as a local single-commit repo, module and build caches warmed.
# Without this it is cloned from GitHub and compiled cold at every instance
# start (~5-10 min on 1 vCPU, network required) — the boot must stay offline
# and fast for BOTH plugins.
# Pinned by commit id, not by tag: a tag can be moved, a commit cannot.
# 4ea454e = the commit our v0.1.0 tag pointed at; upstream is unchanged since.
ARG PLUGINS_REPO=https://github.com/TreveeXYZ/go-evm-indexer-plugins.git
ARG PLUGINS_REF=4ea454e24212511d7c6d165252972e262310067c
RUN git clone --quiet --filter=blob:none ${PLUGINS_REPO} /opt/go-evm-indexer-plugins \
 && cd /opt/go-evm-indexer-plugins \
 && git checkout --quiet ${PLUGINS_REF} \
 && rm -rf .git \
 && git init --quiet --initial-branch=main \
 && git add --all \
 && git -c user.email=build@clear.invalid -c user.name=build commit --quiet -m "baked at image build" \
 && cd plugins/gcp-pubsub \
 && go mod download \
 && go build -o /dev/null .

# envsubst (gettext-base) is the whole rendering machinery.
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends gettext-base \
 && rm -rf /var/lib/apt/lists/*
COPY deploy/config.prod.json /opt/evmi/config.prod.json
COPY deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["entrypoint.sh"]
