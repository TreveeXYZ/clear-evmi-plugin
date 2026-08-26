# clear-defi — EVMI exporter for the Clear protocol

Materializes the [Clear](https://github.com/…/clear-smart-contracts) protocol state into
PostgreSQL from the pipeline's decoded logs: reserves, IOU tokens, Curve StableSwap-NG pools,
all user balances, and the full swap/liquidity history.

Every figure is derived **purely from events**, so:

- balances are exact only when indexing starts **at or before** each contract's first event;
- delivery is at-least-once, but a processed-event ledger makes writes **exactly-once**, so
  balance arithmetic never double-counts on a restart.

The one exception is contract *discovery*: the Curve factory announces a pool deployment without
naming the pool, so that address is resolved over the chain's RPC endpoint and the pool is
registered for indexing through the host API — see
[Curve pools discover themselves](#curve-pools-discover-themselves).

## What it computes

| Table | Content |
|-------|---------|
| `clear_contracts` | address → kind routing registry (`base_reserve`, `meta_reserve`, `iou`, `curve`, `oracle`, `factory`, `curve_deployer`, `curve_factory`) |
| `clear_tokens` | ERC20 directory: every token address the protocol touches — reserve assets, IOUs, reserve LP tokens, Curve pool LP tokens and their coins — with `name`/`symbol`/`decimals` read off the chain on first sight |
| `clear_reserves` | per reserve: `kind` (base/meta), `lp_supply`, cumulative `total_deposits`/`total_withdrawals`, `iou_minted`/`iou_redeemed`, `swap_count`, plus `name`/`symbol`/`implementation`/`tokens` from the factory's `NewClearReserve` |
| `clear_reserve_settings` | per reserve: every governance parameter, folded from the `set*` events (fees, distributions, swap-spread window, rebalance trigger, deposit-weight tolerance) |
| `clear_reserve_lp_balances` | `(reserve, holder) → balance` — every LP holder |
| `clear_reserve_assets` | reserve's assets + their IOU token and `iou_supply` (from `AssetAdded`); for a **meta** reserve, its two legs with each leg's target `weight` in bps |
| `clear_reserve_token_balances` | a reserve's physical ERC20 holdings per asset (base reserves per asset, meta reserves per leg), reconstructed from event token-flows |
| `clear_reserve_swaps` | depeg `Swap` history (amounts + IOU split) |
| `clear_reserve_activity` | deposits, withdrawals, single-asset ops, rebalances, IOU mint/redeem, flash loans |
| `clear_reserve_value_history` | daily end-of-day `total_assets` / `iou_debt` / `nav` / `total_supply` per reserve, both types (for TVL and share-price charts) |
| `clear_reserve_par_values`, `clear_reserve_values` | *views* — live reserve valuation mirroring each contract's own views; a meta reserve's BaseLP leg is priced at the base reserve's NAV/share |
| `clear_iou_tokens` / `clear_iou_balances` | IOU supply, cumulative `treasury_fees`, and per-holder balances |
| `clear_oracle_prices` | per asset: price, redemption price, TTL, decimals, enabled, last refresh — folded from `ClearOracle` + `PythOracleAdapter` |
| `clear_oracle_price_history` | every `ClearOracleRateChanged` as a price point (for charts) |
| `clear_protocol_config` | the factory's protocol-wide config: `treasury` and each clone implementation + version |
| `clear_curve_pools` | per pool: `lp_supply`, `swap_count`, the reserve / base pool / paired coin it was deployed for, plus `name`/`symbol`/`decimals`/`amplification`/`fee`/`implementation`/`is_meta` read back when the Curve factory announced it |
| `clear_curve_pool_coins` | `(pool, position) → coin, decimals` — a pool's coins in slot order (the order `TokenExchange`'s `sold_id`/`bought_id` index into) |
| `clear_curve_lp_balances` | `(pool, holder) → balance` |
| `clear_curve_swaps` | `TokenExchange` / `TokenExchangeUnderlying` history |
| `clear_curve_liquidity` | add/remove liquidity history (token amounts as JSON, `token_supply`) |

Every table carries `chain_id`, so one database can hold more than one chain.

Amounts are `NUMERIC` (uint256-safe); addresses are stored lowercased.

## Build

EVMI runs an exporter plugin as a **subprocess** and talks to it over gRPC
([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin)), so this is an ordinary Go
program — no `-buildmode=plugin`, no matching the server's toolchain or dependency
versions, and a panic here takes down only this process:

```bash
go build -o clear-defi .
```

The repo root **is** the build target: EVMI clones the repository and runs `go build` on
it, so the `main` package must stay at the root (it does — `main.go` calls
`exporter.Serve`). In practice you never build by hand: point EVMI at the git URL and let
it install (below).

Two rules the plugin has to keep holding to:

- **Nothing on stdout.** Stdout carries the go-plugin handshake and the gRPC stream;
  writing to it corrupts the connection. `Init` logs via the standard library `log`
  package, which goes to **stderr** — EVMI captures that and folds it into its own log.
- **Idempotent on `LogEvent.Id`.** Delivery is at-least-once; the `clear_processed_events`
  ledger claims each id inside the same transaction as the writes, so a replayed log is a
  no-op (see `NewLogEvent` in `main.go`).

The only compatibility constraint left is the SDK protocol version: EVMI rejects a plugin
built against an incompatible `pkg/exporter` at handshake time, naming the mismatch. If
that happens, `go get -u github.com/evmi-cloud/go-evm-indexer` and reinstall.

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
   - `Reserve` + `Factory` → `ClearReserveFactory`; `Deployer` → `ClearCurvePoolDeployer`;
     `Curve`/`StableSwap` + `Factory` → `CurveStableSwapFactoryNG` (all matched *before* the
     generic reserve/curve cases)
2. **Blockchain**, **log store**, **pipeline** — as usual.
3. **Sources** — add a source per contract (CONTRACT), plus FACTORY sources for the contracts that
   spawn others: each reserve spawns its `ClearIOU`s via `AssetAdded`, and the
   `ClearCurvePoolDeployer` spawns `CurveStableSwapNG` pools via `PoolDeployed`. The
   `ClearReserveFactory` cannot be a FACTORY source — it deploys two different child ABIs
   (base and meta reserves) and a factory source spawns only one — so each reserve needs its own
   source entry, though the exporter still registers it in `clear_contracts` from
   `NewClearReserve`. All sources must be in the **same pipeline** the exporter reads.
4. **Plugin** — create a `Plugin` record with `GitUrl` = this repository (and optionally
   `GitRef` = a branch or tag), then **Install** it: EVMI clones the repo, runs `go build`
   on its root, stores the executable, and runs it once to read `ConfigSchema()` — which is
   what makes the exporter form typed and validates `pluginConfig` on save. The build runs
   on the EVMI instance, so it needs network access to fetch this module's dependencies.
   Editing the source resets the plugin to `NOT_INSTALLED`; reinstall to rebuild.
5. **Exporter** — create an `EvmiExporter` bound to the pipeline with config:

   ```json
   {
     "dsn": "postgres://user:pass@postgres:5432/clear?sslmode=disable",
     "autoMigrate": true
   }
   ```

   Start it; it processes the pipeline's logs in block order and fills the tables.

### Curve pools discover themselves

Add the **`CurveStableSwapFactoryNG` as a plain `CONTRACT` source** and every pool deployed
through it is picked up on its own — no source entry, no `contracts` entry per pool.

It cannot be a `FACTORY` source: `PlainPoolDeployed` and `MetaPoolDeployed` carry the coins, `A`,
the fee and the deployer, but **not the address of the pool they just created**, so there is
nothing for a creation rule to read. Instead the exporter resolves it itself, using the host API
(EVMI ≥ 0.3.0):

1. `Host.Blockchain()` gives the JSON-RPC endpoint the indexer already polls, which the exporter
   dials with [`lmittmann/w3`](https://github.com/lmittmann/w3);
2. the pool address is recovered from the deployment **transaction receipt** — every
   StableSwap-NG pool fires `Transfer(0x0, factory, 0)` from its constructor, so the pool is the
   emitter of the last such log before the deploy event. This needs no archive node, and stays
   exact when one transaction deploys several pools (the `ClearCurvePoolDeployer` does);
3. `name`/`symbol`/`decimals` come from the pool, and the coins, their decimals, the blueprint
   implementation and `is_meta` from the factory's registry view — all seven getters in a single
   batched request. Each is best-effort: one that reverts leaves its own column NULL without
   costing the others;
4. `Host.CreateLogSource` registers the pool as a child source of the factory, from the
   deployment block, decoding with the `CurvePoolAbi` ABI — which is what makes its swaps,
   liquidity events and LP transfers start arriving. The call is idempotent per
   (parent, address), so a redelivered deployment log never creates a duplicate.

Optional `pluginConfig` keys for this path:

| Key | Default | Meaning |
|-----|---------|---------|
| `rpcUrl` | the endpoint EVMI polls | override, for a plugin that should not share that node |
| `curvePoolAbi` | `CurveStableSwapNG` | ABI the created pool sources decode with; registered from the copy embedded in this plugin if the server does not have it |
| `indexCurvePools` | `true` | set `false` to record the pool in `clear_curve_pools` but leave indexing it to a hand-declared source |

On an EVMI older than the host API (or with no reachable RPC endpoint) this degrades to a warning
on stderr: everything else still works, pools just have to be declared by hand as before.

> Array ABI args (`amounts[]`, `token_amounts`/`fees`, `tokens[]`) arrive in one of two
> renderings depending on the server version — a JSON array, or Go's `fmt.Sprint` form
> (`[1 2]`, which is *not* JSON). Both are accepted; see `splitArrayArg` in `db.go`.

## One-shot setup with the autoloader

`autoload.config.json` in this folder wires the whole thing declaratively — pass it as the EVMI
server config and every resource below is created on startup if absent (idempotent):

- the nine **ABIs** (`ClearBaseReserve`, `ClearMetaReserve`, `ClearIOU`, `ClearReserveFactory`,
  `ClearCurvePoolDeployer`, `CurveStableSwapNG`, `CurveStableSwapFactoryNG`, `ClearOracle`,
  `PythOracleAdapter` — generated from the deployed artifacts), a **blockchain** (sepolia), a
  **log store** (clickhouse), a
  **pipeline** (`clear`), and the **exporter** (with its Postgres `dsn` and the address→kind
  `contracts` registry);
- the **base and meta reserves as `FACTORY` sources** — each indexes the reserve's own events *and*
  auto-creates a `ClearIOU` `CONTRACT` child source for every IOU announced via `AssetAdded`
  (`factoryCreationAddressLogArg: "iou"`);
- the **`ClearCurvePoolDeployer` as a `FACTORY` source** — every pool it deploys becomes a
  `CurveStableSwapNG` child source (`PoolDeployed` / `pool`);
- the **`CurveStableSwapFactoryNG` as a `CONTRACT` source** — the exporter resolves each pool it
  announces over RPC and creates the child source itself (see above);
- the **`ClearReserveFactory`**, both oracle contracts and any externally-deployed Curve pool as
  plain `CONTRACT` sources;
- the plugin itself (via `plugins`: `gitUrl` + optional `gitRef`) — the server clones and
  builds it on startup, then keeps it installed.

**Replace the placeholders first** (they're marked with `0x…`/`<...>`): the RPC URL and key, the
reserve/pool addresses, each source's `startBlock` (set to the deployment block for exact
balances), the metadata + exporter Postgres DSNs, and the plugin `gitUrl`/`gitRef`. Then:

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

- **Reserve on-chain token holdings** (`clear_reserve_token_balances`) are reconstructed for both
  reserve types by applying every event's token-flow as a signed delta — exact only when indexing
  starts at the reserve's deployment. A meta reserve's `Deposit`/`Withdraw` names no address (it
  carries `baseLpIn`/`nativeIn` scalars), so its legs are resolved first: the BaseLP leg is the one
  whose asset **is** a base reserve, the native leg is the other.
- **A meta reserve's value follows the base reserve's NAV**, so its `clear_reserve_value_history`
  row is only refreshed when the *meta* reserve itself emits an event. Between two meta events a
  base-side move leaves the stored figure stale — inherent to daily event-driven snapshots. The
  `clear_reserve_values` view is always live.
- **Reserve settings** are only observable once a `set*` function is actually called — the
  contract defaults are not emitted at `initialize`, so a column in `clear_reserve_settings` stays
  NULL until then (see `RESERVE-SETTINGS.md` in the contracts repo for the defaults).
- Curve LP `lp_supply` is tracked from the pool's ERC20 `Transfer`s (mint/burn); the `token_supply`
  reported by each liquidity event is also stored per-row in `clear_curve_liquidity`.
- **Curve pool discovery is the only path that touches RPC**, and only on a deployment — never
  per event. It needs a full node (receipts + current-state `eth_call`, no archive state), and it
  shares the endpoint the indexer polls, so it is deliberately two round trips per pool: the
  receipt, then every getter in one batch. A pool metadata getter that reverts leaves its own
  column NULL and costs the rest of the batch nothing; an unresolvable address is a warning, while
  a transport failure fails the event so EVMI redelivers it.
- If indexing starts mid-history, holders who received tokens before the start block can show
  negative balances — start at the deployment block for exact figures.
