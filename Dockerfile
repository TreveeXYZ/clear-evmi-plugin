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
ARG EVMI_REPO=https://github.com/TreveeXYZ/go-evm-indexer.git
ARG EVMI_REF=b3e187c9c01fa2057acd4480380d369c998de7fb
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
ARG PLUGINS_REPO=https://github.com/TreveeXYZ/go-evm-indexer-plugins.git
ARG PLUGINS_REF=v0.1.0
RUN git clone --quiet --depth 1 --branch ${PLUGINS_REF} ${PLUGINS_REPO} /opt/go-evm-indexer-plugins \
 && cd /opt/go-evm-indexer-plugins \
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
