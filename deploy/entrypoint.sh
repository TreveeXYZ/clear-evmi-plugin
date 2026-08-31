#!/bin/sh
# Renders the committed config template with the three secret values injected
# as environment variables (Secret Manager -> Cloud Run secret_env), then hands
# off to the server. The rendered file lives in tmpfs only — never on an image
# layer, never in a volume.
set -eu

: "${EVMI_DB_DSN:?EVMI_DB_DSN is required (Postgres DSN for the indexer database)}"
: "${CLEAR_DB_DSN:?CLEAR_DB_DSN is required (Postgres DSN for the clear_* state database)}"
: "${RPC_URL:?RPC_URL is required (chain RPC endpoint)}"

mkdir -p /tmp/evmi
envsubst '$EVMI_DB_DSN $CLEAR_DB_DSN $RPC_URL' \
  < /opt/evmi/config.prod.json > /tmp/evmi/config.json

exec evm-indexer start -c /tmp/evmi/config.json
