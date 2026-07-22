# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

An **EVMI exporter plugin** (Go, `package main`, built with `-buildmode=plugin`) for the **Clear DeFi protocol**. It consumes a pipeline's decoded EVM logs *in block order* and materializes protocol state into PostgreSQL. The plugin gets **no RPC access** — every table is derived purely from events. It implements the `exporter.Exporter` interface from `github.com/evmi-cloud/go-evm-indexer/pkg/exporter`; the EVMI server looks up the exported `New()` symbol to instantiate it (`main.go`). `func main()` exists only because `-buildmode=plugin` requires it and is never run.

## Commands

```bash
# Build the plugin (.so). MUST use the same Go toolchain + module versions as the EVMI server,
# or the server cannot load the plugin.
go build -buildmode=plugin -o clear-defi.so .

go vet ./...
go test ./...                    # unit tests only (classify/neg/firstArg/... in helpers_test.go)
go test ./... -run TestClassify  # a single test

# Integration test — requires a real Postgres (uses NUMERIC / JSONB / GREATEST / ON CONFLICT;
# SQLite won't do). It DROPs all clear_* tables first, so point it at a scratch DB.
CLEAR_DEFI_POSTGRES_DSN='postgres://evmi:evmi@localhost:5432/evmi?sslmode=disable' \
  go test ./... -run TestReplayProtocol -v
```

`autoload.config.json` wires the entire EVMI deployment declaratively (ABIs, blockchain, log store, pipeline, sources, plugin, exporter) — see README for the placeholders to replace before use.

## Architecture and invariants

The whole pipeline is small (`main.go`, `schema.go`, `db.go`, `handlers.go`) but three cross-cutting rules make it work; understand these before changing any handler.

**1. Contract classification is by ABI name, not address.** `classify()` in `db.go` maps a log's `ContractName` to a `contractKind` (base/meta reserve, IOU, curve) via **case-insensitive substring** matching (`…Base…Reserve`, `…Meta…Reserve`, `…IOU…`, `…Curve…`/`…StableSwap…`/`…Pool…`). This is why the *same* event name (notably `Transfer`) can route to three different balance tables. When configuring EVMI, source ABIs **must be named** to match the classifier. New contract types mean extending `classify()`.

**2. Exactly-once via a processed-events ledger.** EVMI delivery is at-least-once. `NewLogEvent` (`main.go`) opens a transaction, does `INSERT INTO clear_processed_events (id) ... ON CONFLICT DO NOTHING`, and if the row already existed (RowsAffected == 0) it **rolls back and does nothing** — the log was already applied. Only new ids reach `dispatch()`. This is what keeps balance arithmetic (which is *additive*, `balance = balance + delta`) from double-counting on a restart/redelivery. Every handler runs inside this one transaction (`tx`); never open a second DB connection or commit mid-handler. The `id` is evmi's stable `chainId:block:logIndex`.

**3. Two write styles, deliberately different.**
   - **Balances & supplies are cumulative deltas** — ERC20 `Transfer` is decomposed into mint (from zero addr → +supply), burn (to zero addr → −supply), or transfer (−from/+to), and `adjustBalance` upserts `balance + delta`. These are correct *only* if indexing starts at/before the contract's first event; starting mid-history yields negative balances (documented limitation).
   - **History rows are idempotent inserts** — swaps, activity, curve liquidity all use `INSERT ... ON CONFLICT (id) DO NOTHING`, keyed by the event id. Safe to reapply.

`dispatch()` in `main.go` is the single event→handler router; unhandled events (Approval, fee/config updates) fall through and are ignored. Adding an event = add a `case` there + a handler in `handlers.go` + (usually) a column/table in `schema.go`.

## Conventions that matter

- **Addresses** are always lowercased via `normAddr` before use as a key (evmi emits mixed-case checksummed hex). The zero address is the ERC20 mint/burn counterparty (`isZeroAddr`).
- **Solidity vs Vyper arg names**: the same field is named differently across contracts (`from`/`to` vs `sender`/`receiver`). `firstArg(args, keys...)` returns the first present key — this is why Curve (Vyper) transfers and reserve (Solidity) transfers both work through one `handleTransfer`.
- **Amounts** are `NUMERIC` (uint256-safe) throughout; helpers (`num`, `numOrZero`, `nullNum`, `neg`) coerce possibly-absent string args into decimal strings/`sql.NullString`. Curve `uint256[]` args (`token_amounts`, `fees`) arrive as JSON arrays of decimal strings and go into `JSONB` via `jsonArg` — this requires an EVMI server that serializes array ABI args.
- `schema.go` holds the entire DDL as one `const schema` string, applied on `Init` when `autoMigrate` (config, default true). All tables are `CREATE TABLE IF NOT EXISTS`; changing the schema of an existing table needs a real migration, not an edit here.

## Reserve physical token balances

`clear_reserve_token_balances (reserve, asset) → balance` reconstructs a **base** reserve's real ERC20 holdings (`balanceOf(reserve)`) purely from event token-flows — distinct from the LP/NAV figures in `clear_reserves`. Every event that moves an underlying token is applied as a signed delta (`adjustTokenBalance`): `Deposit`/`Withdraw` (`amounts[]` array, per-asset), `SingleAsset*` (amount ∓ fee), `Rebalanced`/`Swap` (+in/−out), `IOUMinted`/`IOURedeemed` (±amount, 1:1 backing), `FlashLoan` (+fee only — principal round-trips in-tx).

Two things make it work: (1) the `Deposit`/`Withdraw` `amounts` array is aligned with the contract's append-only `assetList`, so `clear_reserve_assets.position` (assigned from the asset count at `AssetAdded` time) maps `amounts[i]` back to its token; (2) all deltas are gated to `k == baseReserveKind` — **meta reserves are not tracked** (they hold base-LP + native, which aren't in the `AssetAdded` registry, and their `Deposit` carries no `amounts`). Exact only when indexing starts at the reserve's deployment (no missed flow, no missed `AssetAdded`).

The plugin does **not** reconstruct meta-reserve token holdings or the underlying tokens' own ERC20 `Transfer`s (those aren't reserve events).

`clear_reserve_assets.iou_supply` mirrors each asset's IOU `total_supply`: the IOU `Transfer` mint/burn path in `handleTransfer` updates both `clear_iou_tokens` (keyed by IOU address) and the registry row (`WHERE iou = <addr>`) in lockstep, so per-asset IOU supply is available without a join.

## Oracle state

`clear_oracle_prices` is **one row per asset** (PK = `asset`), folding events from two contracts (both classify as `oracleKind` — name contains "oracle"):
- **ClearOracle** — `OracleConfigured` (enabled, asset/oracle decimals, `price_ttl`, `redemption_price`, and sets `oracle` = the ClearOracle address), `ClearOracleRateChanged` (`price`), `ClearOracleRedemptionPriceChanged` (`redemption_price`).
- **PythOracleAdapter** — `PriceUpdated` (`price`).

`last_refresh` is the **last refresh date**: every oracle handler stamps it with `log.BlockTimestamp` (block-header unix seconds — needs an EVMI server new enough to populate that field; see the pinned module version in `go.mod`), and `last_block` gets the same event's block number. The refresh cycle emits `ClearOracleRateChanged` (inside `updateCustomOraclePrice`) *and* the adapter's `PriceUpdated` in the same tx; both fold into the asset's row. Each handler upserts only its own columns (`INSERT … ON CONFLICT (asset) DO UPDATE`) so the row is created or patched in any order — except `handleOraclePublish` deliberately does **not** touch `oracle` (its `log.Address` is the adapter, not the ClearOracle). `parseBool` in `handlers.go` decodes the indexed `enabled` arg across the string forms evmi may emit.
