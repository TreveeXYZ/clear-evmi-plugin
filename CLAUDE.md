# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

An **EVMI exporter plugin** for the **Clear DeFi protocol** — an ordinary Go program (`package main`, plain `go build`), not a `-buildmode=plugin` shared object. EVMI launches it as a **subprocess** and calls it over gRPC via [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin), so its toolchain and dependency versions are its own business and a panic here kills only this process. It consumes a pipeline's decoded EVM logs *in block order* and materializes protocol state into PostgreSQL. **Every table figure is derived purely from events** — the only RPC the plugin makes is on Curve pool *discovery* (see the host API note below), never per event.

It implements `exporter.Exporter` (`Name`/`Init`/`NewLogEvent`/`Close`) plus the optional `exporter.Configurable` (`ConfigSchema`, which EVMI extracts at install time by running the binary once, and validates each exporter's `pluginConfig` against) from `github.com/evmi-cloud/go-evm-indexer/pkg/exporter`; `func main()` hands the implementation to `exporter.Serve`, which blocks until EVMI disconnects. Two constraints follow from the subprocess model: the `main` package must live at the **repo root** (that is the path EVMI clones and builds), and **nothing may be written to stdout** — stdout is the handshake/gRPC channel, so all logging goes to stderr via the standard library `log` package (EVMI captures it into its own log).

## Commands

```bash
# Plain go build — the repo root is the build target EVMI itself uses.
go build -o clear-defi .

go vet ./...
go test ./...                    # unit tests only (helpers_test.go, curve_test.go — the latter
                                 # covers the Transfer binding and pool-address resolution
                                 # against an httptest stand-in for the RPC node)
go test ./... -run TestClassify  # a single test

# Integration test — requires a real Postgres (uses NUMERIC / JSONB / GREATEST / ON CONFLICT;
# SQLite won't do). It DROPs all clear_* tables first, so point it at a scratch DB.
CLEAR_DEFI_POSTGRES_DSN='postgres://evmi:evmi@localhost:5432/evmi?sslmode=disable' \
  go test ./... -run TestReplayProtocol -v

# Bump the SDK (the one compatibility surface left: EVMI rejects a plugin built against an
# incompatible protocol version at handshake, naming the mismatch). NOTE the indexer's
# release tags are `0.3.0`, not `v0.3.0` — not valid Go module versions, so `go list -m
# -versions` shows none of them and `@0.3.0` does not resolve. Map the release to its
# commit and pin the pseudo-version:
#   git ls-remote --tags --refs https://github.com/evmi-cloud/go-evm-indexer.git
go get -u github.com/evmi-cloud/go-evm-indexer && go mod tidy
```

Dependencies are `lib/pq`, the EVMI SDK, and [`lmittmann/w3`](https://github.com/lmittmann/w3)
(chain access — RPC client, ABI bindings; it pulls in go-ethereum). Note `go get -u` will raise the
`go` directive past `1.24.9`, so bump deliberately and pin.

`autoload.config.json` wires the entire EVMI deployment declaratively (ABIs, blockchain, log store, pipeline, sources, plugin, exporter). Its `plugins` entry is `{name, description, gitUrl, gitRef}` — git is the only supported source and the build target is always the repo root, so there is no package path to configure. See README for the placeholders to replace before use.

## Architecture and invariants

The whole pipeline is small (`main.go`, `registry.go`, `dispatch.go`, `schema.go`, `db.go`, `handlers.go`, plus `host.go`/`curve.go` for the one path that reaches out of the process) but three cross-cutting rules make it work; understand these before changing any handler.

**1. Routing is by address, via a registry — classification by ABI name happens at most once per contract.** `NewLogEvent` (`main.go`) resolves a log's `contractKind` by looking its **address** up in an in-memory registry (`clearExporter.registry`), not by re-deriving it from the ABI name on every event. The registry is seeded on `Init` from `pluginConfig.contracts` (`[{address, kind}]`, `kind` ∈ base_reserve/meta_reserve/iou/curve/oracle/factory/curve_deployer/curve_factory) and reloaded from the `clear_contracts` table, so contracts discovered on earlier runs survive restarts (`registry.go`: `seedContracts`/`loadRegistry`/`resolveKind`/`trackContract`). On the **first sight** of an unregistered address, `resolveKind` falls back to `classify()` (the case-insensitive substring matcher in `db.go`: `…Oracle…`, `…Curve…Factory…`/`…StableSwap…Factory…`, `…Deployer…`, `…Reserve…Factory…`, `…Base…Reserve`, `…Meta…Reserve`, `…IOU…`, `…Curve…`/`…StableSwap…`/`…Pool…` — **order matters**, the deployer/factory names also contain "pool"/"reserve"/"curve"), caches the result, and persists a `source='dynamic'` row — so `classify()` runs once per address, never per event.

**Discovery events register contracts eagerly**, each inside the handling tx: `dispatchReserve` calls `trackContract(iou, iouKind)` on `AssetAdded` (both reserve types spawn IOUs); `handleNewClearReserve` registers the reserve the `ClearReserveFactory` announces, with the kind from its `reserveType` enum; `handlePoolDeployed` registers the pool the `ClearCurvePoolDeployer` announces; `handleCurvePoolDeployed` registers the pool the `CurveStableSwapFactoryNG` announces (address resolved over RPC — see below). `clear_contracts` (PK `(chain_id, address)`) is the source of truth; source ABIs still need sensible names for the first-sight fallback, and a new contract type means extending `classify()` **and** `kindFromString`/`contractKind.String()`. Untracked addresses are ignored (their event id is still marked processed).

Registering an address only fixes its **routing** — evmi still has to deliver its logs, which needs a pipeline source for it. `autoload.config.json` runs each reserve and the pool deployer as `FACTORY` sources so their children are auto-sourced; the `ClearReserveFactory` can't be one (a factory source spawns a single child ABI, and it deploys both reserve types), so reserves need their own source entries. Curve pools no longer need one either — the plugin creates their source itself (below).

> **Host API use (SDK ≥ 0.3.0) — `host.go`.** `Context.Host` is a reverse channel from the plugin back into EVMI: `Blockchain()` (chain info + the `RpcUrl` the indexer polls), `CreateLogSource()` (register a contract to index as a child of an existing source, idempotent per `(Parent, Address)`), and `UpsertAbi`/`GetAbi`/`GetAbiByID`/`ListAbis`. The plugin uses exactly two of them, on the Curve factory path only (see "Curve pool discovery" below); it is **nil on older servers**, and every use is behind a nil check that degrades to a stderr warning, so the plugin still runs unchanged on a pre-host-API server. Still unused, if wanted later: `handleNewClearReserve` already resolves the reserve address *and* its base/meta kind, so it could `CreateLogSource` the reserve itself (parent = `log.SourceId`, ABI per kind) and retire the hand-written per-reserve source entries in `autoload.config.json` — the exact gap the two-child-ABI problem above leaves open. The RPC endpoint would also make meta-reserve balance tracking possible (see that limitation below), which is a bigger change than discovery and deliberately not attempted.

**2. Exactly-once via a processed-events ledger.** EVMI delivery is at-least-once. `NewLogEvent` (`main.go`) opens a transaction, does `INSERT INTO clear_processed_events (id) ... ON CONFLICT DO NOTHING`, and if the row already existed (RowsAffected == 0) it **rolls back and does nothing** — the log was already applied. Only new ids reach `dispatch()`. This is what keeps balance arithmetic (which is *additive*, `balance = balance + delta`) from double-counting on a restart/redelivery. Every handler runs inside this one transaction (`tx`); never open a second DB connection or commit mid-handler. The `id` is evmi's stable `chainId:block:logIndex`.

**3. Two write styles, deliberately different.**
   - **Balances & supplies are cumulative deltas** — ERC20 `Transfer` is decomposed into mint (from zero addr → +supply), burn (to zero addr → −supply), or transfer (−from/+to), and `adjustBalance` upserts `balance + delta`. These are correct *only* if indexing starts at/before the contract's first event; starting mid-history yields negative balances (documented limitation).
   - **History rows are idempotent inserts** — swaps, activity, curve liquidity all use `INSERT ... ON CONFLICT (id) DO NOTHING`, keyed by the event id. Safe to reapply.

`dispatch()` in `dispatch.go` is a method on `*clearExporter` that routes by the already-resolved `contractKind` into a **per-contract dispatcher** (`dispatchReserve`/`dispatchIOU`/`dispatchCurve`/`dispatchOracle`/`dispatchFactory`/`dispatchCurveDeployer`/`dispatchCurveFactory`), each of which switches only over the events its contract emits; unhandled events (Approval, Paused/Unpaused, AuthorityUpdated, the adapter's `PriceIdSet`/`OracleUpdated`/`MaxPriceAgeUpdated`, the batch router entirely) fall through and are ignored. Adding an event = add a `case` to the relevant per-contract dispatcher + a handler in `handlers.go` + (usually) a column/table in `schema.go`. Adding a whole contract type also touches the registry (`classify`, `kindFromString`, `contractKind.String()`) and `dispatch`.

The ABIs in `autoload.config.json` are generated from the contracts repo's deployed artifacts (`ignition/deployments/chain-*/artifacts/*.json`) and are event-only. When the contracts change their logs, regenerate them from those artifacts rather than hand-editing — a missing event is silent (evmi just never decodes it).

## Conventions that matter

- **Multi-chain: every table carries `chain_id`.** One database can hold state for more than one chain, so `chain_id` (from `log.ChainId`, or `ctx.ChainId` in `Init`) is on every table and is threaded into every INSERT/UPDATE/WHERE/`ON CONFLICT` and every cross-table join. For entity/address-keyed tables (`clear_reserves`, all `*_balances`, `clear_reserve_assets`, `clear_reserve_token_balances`, `clear_oracle_prices`, `clear_curve_pools`, `clear_reserve_value_history`, `clear_reserve_settings`, `clear_protocol_config`, `clear_contracts`) `chain_id` is **part of the PRIMARY KEY** — the same address can exist on two chains (deterministic CREATE2), so it must not collide. History tables keyed by the event `id` (already `chainId:block:logIndex`, globally unique) keep `id` as the sole PK and carry `chain_id` only as a filter column. Helpers (`ensureReserve`/`ensureCurve`/`ensureIou`/`adjustBalance`/`adjustTokenBalance`/`bumpReserve`/`reserveAssetsByPosition`/`applyReserveAmounts`) all take `chainID uint64`. Note: adding `chain_id` changed existing PKs, and `CREATE TABLE IF NOT EXISTS` won't migrate a pre-existing table — an existing deployment needs a real migration (or drop the `clear_*` tables on a scratch DB).
- **Tracked-contracts registry** — `clear_contracts (chain_id, address) → (kind, source, first_block)` is the routing registry (see invariant 1). `source='config'` rows come from `pluginConfig.contracts` at `Init` (config wins over a prior dynamic row); `source='dynamic'` rows are added at runtime (`trackContract`/`resolveKind`) with `ON CONFLICT DO NOTHING` so the first writer's row stands. The in-memory `registry` is a per-chain `map[address]contractKind` guarded by a mutex.
- **Addresses** are always lowercased via `normAddr` before use as a key (evmi emits mixed-case checksummed hex). The zero address is the ERC20 mint/burn counterparty (`isZeroAddr`).
- **Solidity vs Vyper arg names**: the same field is named differently across contracts (`from`/`to` vs `sender`/`receiver`). `firstArg(args, keys...)` returns the first present key — this is why Curve (Vyper) transfers and reserve (Solidity) transfers both work through one `handleTransfer`.
- **Amounts** are `NUMERIC` (uint256-safe) throughout; helpers (`num`, `numOrZero`, `nullNum`, `neg`) coerce possibly-absent string args into decimal strings/`sql.NullString`.
- **Array ABI args have TWO renderings and both must be accepted** (`splitArrayArg` in `db.go`). Which one arrives depends on the server: a real JSON array (`["1","2"]`) from a server that serializes array args, or Go's `fmt.Sprint` form (`[1 2]`, `[0xAbC… 0xDeF…]`) from one whose `formatArgValue` has no slice case and falls through to `fmt.Sprint`. The current indexer (0.3.0) does the latter — its own test asserts `formatArgValue([]*big.Int{1,2}) == "[1 2]"` — so this is the live behaviour, not an edge case. The `fmt.Sprint` form is **not JSON**: passing it straight to a `JSONB` column fails with `pq: invalid input syntax for type json`, and `json.Unmarshal` on it fails outright. `splitArrayArg` tries JSON first (so `["1","2"]` is never split on whitespace) and falls back to whitespace splitting, keeping numbers as literal text so a uint256 never goes through `float64`. Three args depend on it: reserve `Deposit`/`Withdraw` `amounts[]`, Curve `token_amounts`/`fees`, and `NewClearReserve` `tokens[]`. For the `JSONB` columns, `jsonArrayArg` normalizes either form into a canonical JSON array of strings.
- `schema.go` holds the entire DDL as one `const schema` string, applied on `Init` when `autoMigrate` (config, default true). All tables are `CREATE TABLE IF NOT EXISTS`; changing the schema of an existing table needs a real migration, not an edit here.

## Reserve physical token balances

`clear_reserve_token_balances (reserve, asset) → balance` reconstructs a **base** reserve's real ERC20 holdings (`balanceOf(reserve)`) purely from event token-flows — distinct from the LP/NAV figures in `clear_reserves`. Every event that moves an underlying token is applied as a signed delta (`adjustTokenBalance`): `Deposit`/`Withdraw` (`amounts[]` array, per-asset), `SingleAsset*` (amount ∓ fee), `Rebalanced`/`Swap` (+in/−out), `IOUMinted`/`IOURedeemed` (±amount, 1:1 backing), `FlashLoan` (+fee only — principal round-trips in-tx).

Two things make it work: (1) the `Deposit`/`Withdraw` `amounts` array is aligned with the contract's append-only `assetList`, so `clear_reserve_assets.position` (assigned from the asset count at `AssetAdded` time) maps `amounts[i]` back to its token; (2) all deltas are gated to `k == baseReserveKind`. Exact only when indexing starts at the reserve's deployment (no missed flow, no missed `AssetAdded`).

**Meta reserves are still not balance-tracked**, but the reason has narrowed: they now emit `AssetAdded` once per leg (`AssetAdded(asset, decimals, iou, int256 weight)`, native first then BaseLP), so their legs *are* in `clear_reserve_assets` with each leg's target weight — what's missing is that a meta `Deposit`/`Withdraw` carries named scalars (`baseLpIn`/`nativeIn`, `baseLpOut`/`nativeOut`) instead of a positional `amounts` array, and nothing in the event ties a scalar to a leg address. Partial tracking would produce wrong balances, so none is attempted. The plugin also does not reconstruct the underlying tokens' own ERC20 `Transfer`s (those aren't reserve events).

`clear_reserve_assets.iou_supply` mirrors each asset's IOU `total_supply`: the IOU `Transfer` mint/burn path in `handleTransfer` updates both `clear_iou_tokens` (keyed by IOU address) and the registry row (`WHERE iou = <addr>`) in lockstep, so per-asset IOU supply is available without a join. `clear_iou_tokens.treasury_fees` sums the treasury's cut of each mint — only `ClearIOUMinted` carries that split (the paired `Transfer`s show the combined movement), so it is the one IOU event handled besides `Transfer`.

## Oracle state

`clear_oracle_prices` is **one row per asset** (PK = `asset`), folding events from two contracts (both classify as `oracleKind` — name contains "oracle"):
- **ClearOracle** — `OracleConfigured` (enabled, asset/oracle decimals, `price_ttl`, `redemption_price`, and sets `oracle` = the ClearOracle address), `ClearOracleRateChanged` (`price`), `ClearOracleRedemptionPriceChanged` (`redemption_price`).
- **PythOracleAdapter** — `PriceUpdated` (`price`).

`publish_time` is Pyth's own timestamp for the price (the `publishTime` arg of `PriceUpdated`), i.e. when the feed published it rather than when it landed on-chain — so staleness can be judged against the source, not just against inclusion.

`last_refresh` is the **last refresh date**: every oracle handler stamps it with `log.BlockTimestamp` (block-header unix seconds — needs an EVMI server new enough to populate that field; see the pinned module version in `go.mod`), and `last_block` gets the same event's block number. The refresh cycle emits `ClearOracleRateChanged` (inside `updateCustomOraclePrice`) *and* the adapter's `PriceUpdated` in the same tx; both fold into the asset's row. Each handler upserts only its own columns (`INSERT … ON CONFLICT (asset) DO UPDATE`) so the row is created or patched in any order — except `handleOraclePublish` deliberately does **not** touch `oracle` (its `log.Address` is the adapter, not the ClearOracle). `parseBool` in `handlers.go` decodes the indexed `enabled` arg across the string forms evmi may emit.

## Reserve governance settings

`clear_reserve_settings` is **one row per reserve**, folded from the `set*` events both reserve types emit (`ConfigUpdated`, `FlashFeeUpdated`, `BaseQuoteFeeBpsUpdated`, `SingleAssetFeeBpsUpdated`, `RedemptionProximityBpsUpdated`, `IouDistributionUpdated`, `BaseQuoteDistributionUpdated`, `SwapSpreadBpsUpdated`, plus base-only `RebalanceTriggerBpsUpdated` and meta-only `DepositWeightToleranceBpsUpdated`). `handleReserveSettings` takes a flat list of column/value pairs from the dispatcher and builds the upsert, so each event patches **only its own columns** and the row can be created or updated in any order.

The parameter reference is `RESERVE-SETTINGS.md` in the contracts repo. Two things follow from it: a column is **NULL until its setter is first called** — the contract defaults are not emitted at `initialize`, so absence means "still at the default", not zero; and parameters fixed at `initialize` (`targetBaseLpBps`, the asset set) never appear here — the meta legs' target weights arrive through `AssetAdded` as `clear_reserve_assets.weight` instead.

## Factory and pool-deployer state

- **`clear_protocol_config`** — one row per `ClearReserveFactory` (which *is* the `IClearReserveConfig` every reserve points at): `treasury` (from `NewTreasury`) and each clone type's current implementation + the version the factory bumped it to (`New{BaseReserve,MetaReserve,IOU}Implementation`). Same patch-only-your-columns upsert shape as the settings table.
- **`clear_reserves`** gains `name`/`symbol`/`implementation`/`reserve_index`/`tokens`/`factory` from `NewClearReserve`. They stay NULL for a reserve seen only through its own logs (indexing started after deployment, or the factory isn't a source).
- **`clear_curve_pools`** gains `reserve`/`base_pool`/`coin`/`is_base_pool`/`deployer` from `PoolDeployed`, which links every deployer-built pool back to the reserve it serves. `coin == 0` marks the reserve's plain base pool (there `pool == basePool`); it is stored as `is_base_pool = true` with `coin` NULL. Otherwise the pool is a metapool pairing `coin` — an asset's IOU, or a meta reserve's native token / native IOU — against `base_pool`.

## Curve pool discovery (`curve.go`, `host.go`) — the one path that leaves the process

`CurveStableSwapFactoryNG` catches every pool deployed through Curve's own factory, not just the ones `ClearCurvePoolDeployer` built. Its two deployment events — `PlainPoolDeployed(coins[], A, fee, deployer)` and `MetaPoolDeployed(coin, base_pool, A, fee, deployer)` — **do not carry the address of the pool they created** (`deploy_plain_pool`/`deploy_metapool` return it, they never log it). So no EVMI `FACTORY` source can spawn the child; the factory is a plain `CONTRACT` source and `handleCurvePoolDeployed` does the work:

1. **Resolve the address from the deployment transaction's receipt.** Every StableSwap-NG pool fires `Transfer(0x0, msg.sender, 0)` at the end of its constructor, and `msg.sender` there is the factory (the pool is made with `create_from_blueprint`) — so the pool is the emitter of the **last such log before the deploy event** in the same tx (`resolveDeployedPool`). Chosen over reading `pool_list`/`pool_count`, because it needs **no archive node** (the index the pool got is historical state; the receipt is not) and it stays exact when one tx deploys several pools — which `ClearCurvePoolDeployer` does.
2. **Read back what no log carries** (`fetchCurvePool`): `name`/`symbol`/`decimals` from the pool, and `get_coins`/`get_decimals`/`get_implementation_address`/`is_meta` from the factory's registry view — as **one batched request**, since this is the endpoint the indexer polls. All **best-effort**: w3 fills in every call that succeeded and reports the rest through `w3.CallErrors` (a slice parallel to the calls), so a getter that reverts warns and leaves *only its own* column NULL. The coin list falls back to the event's own args (`eventCoins`). Careful when adding a field: read it out **only** if its slot in `CallErrors` is nil, or a zero value ends up masquerading as data.
3. **Register the pool** in `clear_contracts` (`curveKind`) and in `clear_curve_pools` / `clear_curve_pool_coins`, writing **only its own columns** (COALESCE on `deployer`/`base_pool`/`coin`) so the reserve linkage `PoolDeployed` writes in the same tx survives whichever handler lands second.
4. **`Host.CreateLogSource`** it as a child of the factory source (`Parent = log.SourceId`, `StartBlock = log.BlockNumber`), which is what makes the pool's own events start arriving. Idempotent per `(Parent, Address)`, so a redelivery never duplicates. The ABI id comes from `GetAbi(curvePoolAbi)`, falling back to `UpsertAbi` with the pool ABI embedded in `host.go`.

Error policy matters here: a **transport** failure returns an error, which rolls the tx back (the processed-events claim with it) so EVMI redelivers the block; an **unresolvable** pool (receipt read fine, no candidate) only warns, since retrying cannot help. No host / no RPC endpoint → warn and skip, i.e. the pre-host-API behaviour.

Chain access is [`lmittmann/w3`](https://github.com/lmittmann/w3): `w3.Dial` in `Init` (closed in `Close`), `eth.TxReceipt`/`eth.CallFunc` for the two RPC methods used, and `w3.MustNewFunc`/`w3.MustNewEvent` for ABI bindings written as Solidity signatures — no generated bindings, no hand-rolled ABI coding. Calls always run at `latest`: everything read is fixed at deployment, so no archive node is assumed. **`curve.go` is the only file that knows about w3/go-ethereum types** — it hands the rest of the plugin the same lowercase-hex and decimal-string values the schema uses, which is why `handlers.go` never sees a `common.Address`.

Config: `rpcUrl` (default = `Host.Blockchain().RpcUrl`), `curvePoolAbi` (default `CurveStableSwapNG`), `indexCurvePools` (default true).

## Time-series / history tables (for charting)

- **`clear_oracle_price_history`** — **every** price update, one row per `ClearOracleRateChanged`, keyed by event `id` (`ON CONFLICT (id) DO NOTHING`, redelivery-safe). Recorded in `handleOracleRate`, *not* in `handleOraclePublish`, to avoid a duplicate point per refresh (every price write funnels through `ClearOracleRateChanged`).
- **`clear_reserve_value_history`** — **daily** granularity, PK `(reserve, day)` where `day` is the UTC date of the block timestamp. `total_assets` (= `Σ balance·10^(18−decimals)`, the contract's par-valued `totalAssets()`, computed by a SQL aggregate over `clear_reserve_token_balances`) and `total_supply` (LP supply). `snapshotReserveValue` upserts the day's row; the `ON CONFLICT … WHERE existing.block_number <= EXCLUDED.block_number` guard keeps the highest-block snapshot, so each row is that day's **end-of-day** value. It's called at the end of each **base-reserve** flow handler (deposit/withdraw/single/rebalance/swap/IOU/flash), *after* that event's balance/supply mutations, so each snapshot is internally consistent. It is deliberately **not** called from `handleTransfer`: every reserve LP mint/burn is paired with a flow event in the same block, and snapshotting at the bare `Transfer` would capture new supply against stale assets. Meta reserves are excluded (their holdings aren't tracked).
