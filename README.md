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
| `clear_reserves` | per reserve: `kind` (base/meta), `lp_supply`, cumulative `total_deposits`/`total_withdrawals`, `iou_minted`/`iou_redeemed`, `swap_count` |
| `clear_reserve_lp_balances` | `(reserve, holder) → balance` — every LP holder |
| `clear_reserve_assets` | reserve's underlying assets + their IOU token (from `AssetAdded`) |
| `clear_reserve_swaps` | depeg `Swap` history (amounts + IOU split) |
| `clear_reserve_activity` | deposits, withdrawals, single-asset ops, rebalances, IOU mint/redeem, flash loans |
| `clear_iou_tokens` / `clear_iou_balances` | IOU supply and per-holder balances |
| `clear_curve_pools` | per pool: `lp_supply`, `swap_count` |
| `clear_curve_lp_balances` | `(pool, holder) → balance` |
| `clear_curve_swaps` | `TokenExchange` / `TokenExchangeUnderlying` history |
| `clear_curve_liquidity` | add/remove liquidity history (token amounts as JSON, `token_supply`) |

Amounts are `NUMERIC` (uint256-safe); addresses are stored lowercased.

## Build

Build with the **same Go toolchain and module versions** as the EVMI server:

```bash
go build -buildmode=plugin -o clear-defi.so ./examples/exporters/clear-defi
```

(Or let EVMI build it from source: install a `Plugin` pointing at this git repo / path.)

## Configure in EVMI

1. **ABIs** — create one `EvmJsonAbi` per contract type. **The plugin classifies contracts by
   ABI `ContractName` (case-insensitive substring)**, so name them so they contain:
   - `Base` + `Reserve` → e.g. `ClearBaseReserve` (must include ERC20 `Transfer` + the reserve events)
   - `Meta` + `Reserve` → e.g. `ClearMetaReserve`
   - `IOU` → e.g. `ClearIOU`
   - `Curve` / `StableSwap` / `Pool` → e.g. `CurveStableSwapNG` (include `Transfer`, `TokenExchange`, and the liquidity events)
2. **Blockchain**, **log store**, **pipeline** — as usual.
3. **Sources** — add a source per contract (CONTRACT), or a FACTORY source on
   `ClearReserveFactory` / the Curve deployer with the right child ABI so new reserves/pools are
   picked up automatically. All sources must be in the **same pipeline** the exporter reads.
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

- the four **ABIs** (named for the classifier), a **blockchain** (sepolia), a **log store**
  (clickhouse), a **pipeline** (`clear`), and the **exporter** (with its Postgres `dsn`);
- the **base reserve as a `FACTORY` source** — it indexes the reserve's own events *and*
  auto-creates a `ClearIOU` `CONTRACT` child source for every IOU announced via `AssetAdded`
  (`factoryCreationAddressLogArg: "iou"`); the meta reserve and a Curve pool are plain `CONTRACT`
  sources;
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

- **Reserve on-chain token reserves** (how much USDC/USDT a reserve physically holds) are not
  reconstructed here — that needs the ERC20 `Transfer`s of the *underlying* tokens to/from the
  reserve (index those tokens too if you want it) or periodic RPC snapshots. This plugin tracks LP
  supply, flows, IOU, and per-holder LP/IOU balances, which is what the events give exactly.
- Curve LP `lp_supply` is tracked from the pool's ERC20 `Transfer`s (mint/burn); the `token_supply`
  reported by each liquidity event is also stored per-row in `clear_curve_liquidity`.
- If indexing starts mid-history, holders who received tokens before the start block can show
  negative balances — start at the deployment block for exact figures.
