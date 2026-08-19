# clear-defi — EVMI exporter for the Clear protocol

Materializes the [Clear](https://github.com/…/clear-smart-contracts) protocol state into
PostgreSQL from the pipeline's decoded logs: reserves, IOU tokens, Curve StableSwap-NG pools,
all user balances, and the full swap/liquidity history.

Everything is derived **purely from events** (an exporter plugin gets no RPC access), so:

- balances are exact only when indexing starts **at or before** each contract's first event;
- delivery is at-least-once, but a processed-event ledger makes writes **exactly-once**, so
  balance arithmetic never double-counts on a restart.

## What it computes

| Table | Content |
|-------|---------|
| `clear_contracts` | address → kind routing registry (`base_reserve`, `meta_reserve`, `iou`, `curve`, `oracle`, `factory`, `curve_deployer`) |
| `clear_reserves` | per reserve: `kind` (base/meta), `lp_supply`, cumulative `total_deposits`/`total_withdrawals`, `iou_minted`/`iou_redeemed`, `swap_count`, plus `name`/`symbol`/`implementation`/`tokens` from the factory's `NewClearReserve` |
| `clear_reserve_settings` | per reserve: every governance parameter, folded from the `set*` events (fees, distributions, swap-spread window, rebalance trigger, deposit-weight tolerance) |
| `clear_reserve_lp_balances` | `(reserve, holder) → balance` — every LP holder |
| `clear_reserve_assets` | reserve's assets + their IOU token and `iou_supply` (from `AssetAdded`); for a **meta** reserve, its two legs with each leg's target `weight` in bps |
| `clear_reserve_token_balances` | a **base** reserve's physical ERC20 holdings per asset, reconstructed from event token-flows |
| `clear_reserve_swaps` | depeg `Swap` history (amounts + IOU split) |
| `clear_reserve_activity` | deposits, withdrawals, single-asset ops, rebalances, IOU mint/redeem, flash loans |
| `clear_reserve_value_history` | daily end-of-day `total_assets` / `total_supply` per base reserve (for TVL charts) |
| `clear_iou_tokens` / `clear_iou_balances` | IOU supply, cumulative `treasury_fees`, and per-holder balances |
| `clear_oracle_prices` | per asset: price, redemption price, TTL, decimals, enabled, last refresh — folded from `ClearOracle` + `PythOracleAdapter` |
| `clear_oracle_price_history` | every `ClearOracleRateChanged` as a price point (for charts) |
| `clear_protocol_config` | the factory's protocol-wide config: `treasury` and each clone implementation + version |
| `clear_curve_pools` | per pool: `lp_supply`, `swap_count`, and the reserve / base pool / paired coin it was deployed for |
| `clear_curve_lp_balances` | `(pool, holder) → balance` |
| `clear_curve_swaps` | `TokenExchange` / `TokenExchangeUnderlying` history |
| `clear_curve_liquidity` | add/remove liquidity history (token amounts as JSON, `token_supply`) |

Every table carries `chain_id`, so one database can hold more than one chain.

Amounts are `NUMERIC` (uint256-safe); addresses are stored lowercased.

## Build

A Go plugin loads **only** if, for every package it shares with the host, an 8-byte
`pkghash` is identical — otherwise `plugin.Open` fails with
`plugin was built with a different version of package …`. That hash depends on three
things, all of which must match the EVMI server binary:

1. **the Go release** — `go.mod`'s `go` directive is only a *minimum*; the published
   image is built with a later patch release (go.mod says 1.24.9, the image uses
   go1.24.13);
2. **every shared dependency version** — `go.mod` must resolve exactly what the server
   linked (notably `github.com/lib/pq v1.10.9`; don't let `go get -u` bump it), and the
   `go-evm-indexer` pseudo-version must be the **commit the image was built from**;
3. **the absolute path the indexer source was compiled from** — the indexer builds
   without `-trimpath` from `WORKDIR /app`, so `/app/pkg/exporter/…` is baked into the
   hash. A plugin that pulls the indexer from the module cache compiles the *same source*
   at `/go/pkg/mod/…@v…/pkg/exporter` and gets a **different** hash.

(3) is the one that bites, and it is why `go build -buildmode=plugin` alone is not
enough — including the build EVMI itself runs when it clones the repo and compiles it
in-container. `-trimpath` does not fix it either: a trimpath'd host and a trimpath'd
plugin still disagree, because one compiles the package as the main module and the other
as a dependency. Matching the path is what works.

`build.sh` does all of it, and refuses to emit a `.so` that would not load:

```bash
./build.sh            # -> clear-defi.so, verified against the real server binary
```

It reads the target Go release and indexer commit **off the EVMI image itself**
(`org.opencontainers.image.revision` + the binary's stamp), checks that commit out at
`/app` inside a `golang:<release>-bookworm` container, points the plugin's `go.mod` there
with a `replace`, builds, then diffs every shared `pkghash` against the real
`/evm-indexer` binary. The `replace` is injected into a throwaway copy, so the committed
`go.mod` stays clean and `go test ./...` keeps working normally.

Overridable: `EVMI_IMAGE`, `GO_VERSION`, `INDEXER_REV`, `INDEXER_REPO`, `BUILD_PATH`, `OUT`.

To check an existing `.so` against a server binary:

```bash
docker cp <evmi-container>:/evm-indexer /tmp/evm-indexer
docker run --rm -v /tmp:/w -v "$PWD":/p golang:1.24.13-bookworm \
  bash /p/verify-plugin.sh /w/evm-indexer /p/clear-defi.so
```

Install the result by placing it in the server's plugins directory (the volume mounted at
`/evmi/plugins`, named after the EVMI `Plugin` record, e.g. `Clear.so`) and restarting.

> **Do not let EVMI install this plugin from git.** When the `.so` is missing, the server
> clones `GitUrl` and runs `go build -buildmode=plugin` itself — that build resolves the
> indexer from the module cache, not `/app`, so it always produces a `.so` the same
> server cannot load. Ship the prebuilt `.so` into the volume instead. (The durable
> upstream fix is for the indexer image to keep its source at `/app` in the final stage,
> so the runtime build can `replace` onto it.)

Linux `.so` only, and `CGO_ENABLED=1` is required.

## Configure in EVMI

1. **ABIs** — create one `EvmJsonAbi` per contract type. Routing is by **address**, from the
   registry seeded by `pluginConfig.contracts`; the ABI `ContractName` is only the fallback used
   the first time an unregistered address is seen (case-insensitive substring), so name them so
   they contain:
   - `Base` + `Reserve` → e.g. `ClearBaseReserve` (must include ERC20 `Transfer` + the reserve events)
   - `Meta` + `Reserve` → e.g. `ClearMetaReserve`
   - `IOU` → e.g. `ClearIOU`
   - `Curve` / `StableSwap` / `Pool` → e.g. `CurveStableSwapNG` (include `Transfer`, `TokenExchange`, and the liquidity events)
   - `Oracle` → e.g. `ClearOracle`, `PythOracleAdapter`
   - `Reserve` + `Factory` → `ClearReserveFactory`; `Deployer` → `ClearCurvePoolDeployer`
     (both matched *before* the generic reserve/curve cases)
2. **Blockchain**, **log store**, **pipeline** — as usual.
3. **Sources** — add a source per contract (CONTRACT), plus FACTORY sources for the contracts that
   spawn others: each reserve spawns its `ClearIOU`s via `AssetAdded`, and the
   `ClearCurvePoolDeployer` spawns `CurveStableSwapNG` pools via `PoolDeployed`. The
   `ClearReserveFactory` cannot be a FACTORY source — it deploys two different child ABIs
   (base and meta reserves) and a factory source spawns only one — so each reserve needs its own
   source entry, though the exporter still registers it in `clear_contracts` from
   `NewClearReserve`. All sources must be in the **same pipeline** the exporter reads.
4. **Plugin** — install this plugin (`InstallPlugin`).
5. **Exporter** — create an `EvmiExporter` bound to the pipeline with config:

   ```json
   {
     "dsn": "postgres://user:pass@postgres:5432/clear?sslmode=disable",
     "autoMigrate": true
   }
   ```

   Start it; it processes the pipeline's logs in block order and fills the tables.

> The `uint256[]` args on Curve liquidity events require an EVMI server that serializes array
> ABI args (added alongside this example); older servers panic on them.

## One-shot setup with the autoloader

`autoload.config.json` in this folder wires the whole thing declaratively — pass it as the EVMI
server config and every resource below is created on startup if absent (idempotent):

- the eight **ABIs** (`ClearBaseReserve`, `ClearMetaReserve`, `ClearIOU`, `ClearReserveFactory`,
  `ClearCurvePoolDeployer`, `CurveStableSwapNG`, `ClearOracle`, `PythOracleAdapter` — generated
  from the deployed artifacts), a **blockchain** (sepolia), a **log store** (clickhouse), a
  **pipeline** (`clear`), and the **exporter** (with its Postgres `dsn` and the address→kind
  `contracts` registry);
- the **base and meta reserves as `FACTORY` sources** — each indexes the reserve's own events *and*
  auto-creates a `ClearIOU` `CONTRACT` child source for every IOU announced via `AssetAdded`
  (`factoryCreationAddressLogArg: "iou"`);
- the **`ClearCurvePoolDeployer` as a `FACTORY` source** — every pool it deploys becomes a
  `CurveStableSwapNG` child source (`PoolDeployed` / `pool`);
- the **`ClearReserveFactory`**, both oracle contracts and any externally-deployed Curve pool as
  plain `CONTRACT` sources;
- the plugin itself (via `plugins`, from your git repo).

**Replace the placeholders first** (they're marked with `0x…`/`<...>`): the RPC URL and key, the
reserve/pool addresses, each source's `startBlock` (set to the deployment block for exact
balances), the metadata + exporter Postgres DSNs, and the plugin `gitUrl`. Then:

```bash
go run ./cmd/evm-indexer start --config examples/exporters/clear-defi/autoload.config.json --instance evmi-1
```

The ABI `content` fields are the minimal event-only ABIs the exporter needs (the `indexed` flags
match the contracts, which is how EVMI splits topics from data). If you add events, extend both the
ABI and a handler in `handlers.go`.

## Example queries

Reserve TVL proxy (LP supply) and IOU exposure:

```sql
SELECT address, kind, lp_supply, iou_minted - iou_redeemed AS iou_outstanding, swap_count
FROM clear_reserves ORDER BY lp_supply DESC;
```

A user's positions across reserves and pools:

```sql
SELECT 'reserve' AS venue, reserve AS pool, balance FROM clear_reserve_lp_balances WHERE holder = $1 AND balance <> 0
UNION ALL
SELECT 'curve', pool, balance FROM clear_curve_lp_balances WHERE holder = $1 AND balance <> 0;
```

Recent depeg swaps on a reserve:

```sql
SELECT block_number, trader, token_in, amount_in, token_out, amount_out, iou_total
FROM clear_reserve_swaps WHERE reserve = $1 ORDER BY block_number DESC, log_index DESC LIMIT 50;
```

Curve pool volume by day-block bucket (swaps) and current LP supply:

```sql
SELECT p.address, p.lp_supply, count(s.*) AS swaps, sum(s.tokens_sold::numeric) AS volume_sold
FROM clear_curve_pools p LEFT JOIN clear_curve_swaps s ON s.pool = p.address
GROUP BY p.address, p.lp_supply;
```

## Notes / limitations

- **Reserve on-chain token holdings** are reconstructed for **base** reserves only
  (`clear_reserve_token_balances`), by applying every event's token-flow as a signed delta —
  exact only when indexing starts at the reserve's deployment. A **meta** reserve's holdings are
  not: it now emits `AssetAdded` per leg, so its legs are registered, but its balanced
  `Deposit`/`Withdraw` carry named scalars (`baseLpIn`/`nativeIn`) with nothing tying a scalar to
  a leg address.
- **Reserve settings** are only observable once a `set*` function is actually called — the
  contract defaults are not emitted at `initialize`, so a column in `clear_reserve_settings` stays
  NULL until then (see `RESERVE-SETTINGS.md` in the contracts repo for the defaults).
- Curve LP `lp_supply` is tracked from the pool's ERC20 `Transfer`s (mint/burn); the `token_supply`
  reported by each liquidity event is also stored per-row in `clear_curve_liquidity`.
- If indexing starts mid-history, holders who received tokens before the start block can show
  negative balances — start at the deployment block for exact figures.
