package main

// schema is the full set of tables the exporter writes. Applied on Init when
// autoMigrate is enabled. All amounts are NUMERIC (uint256-safe); addresses are
// stored lowercased. `id` columns are evmi's stable "chainId:block:logIndex".
const schema = `
-- Idempotency: at-least-once delivery means a log may arrive twice; we record
-- every processed event id and skip repeats, so balance math stays exact.
CREATE TABLE IF NOT EXISTS clear_processed_events (
    id TEXT PRIMARY KEY
);

-- Reserves (Clear base/meta reserves; the LP token IS the reserve contract).
CREATE TABLE IF NOT EXISTS clear_reserves (
    address           TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,
    lp_supply         NUMERIC NOT NULL DEFAULT 0,
    total_deposits    NUMERIC NOT NULL DEFAULT 0,
    total_withdrawals NUMERIC NOT NULL DEFAULT 0,
    iou_minted        NUMERIC NOT NULL DEFAULT 0,
    iou_redeemed      NUMERIC NOT NULL DEFAULT 0,
    swap_count        BIGINT  NOT NULL DEFAULT 0,
    first_block       BIGINT,
    last_block        BIGINT
);

CREATE TABLE IF NOT EXISTS clear_reserve_lp_balances (
    reserve    TEXT NOT NULL,
    holder     TEXT NOT NULL,
    balance    NUMERIC NOT NULL DEFAULT 0,
    last_block BIGINT,
    PRIMARY KEY (reserve, holder)
);
CREATE INDEX IF NOT EXISTS clear_reserve_lp_balances_holder ON clear_reserve_lp_balances (holder);

-- Per-reserve asset registry (from AssetAdded), including the asset's IOU token.
CREATE TABLE IF NOT EXISTS clear_reserve_assets (
    reserve  TEXT NOT NULL,
    asset    TEXT NOT NULL,
    decimals INT,
    iou      TEXT,
    PRIMARY KEY (reserve, asset)
);

-- Depeg-swap history (the Swap event).
CREATE TABLE IF NOT EXISTS clear_reserve_swaps (
    id           TEXT PRIMARY KEY,
    reserve      TEXT NOT NULL,
    block_number BIGINT NOT NULL,
    log_index    BIGINT NOT NULL,
    tx_hash      TEXT,
    trader       TEXT,
    token_in     TEXT,
    token_out    TEXT,
    recipient    TEXT,
    amount_in    NUMERIC,
    amount_out   NUMERIC,
    iou_total    NUMERIC,
    trader_iou   NUMERIC,
    treasury_iou NUMERIC,
    lp_iou       NUMERIC
);
CREATE INDEX IF NOT EXISTS clear_reserve_swaps_reserve_block ON clear_reserve_swaps (reserve, block_number);

-- Everything else that happens on a reserve (deposits, withdrawals, rebalances,
-- IOU mint/redeem, flash loans) as a unified activity log.
CREATE TABLE IF NOT EXISTS clear_reserve_activity (
    id           TEXT PRIMARY KEY,
    reserve      TEXT NOT NULL,
    block_number BIGINT NOT NULL,
    log_index    BIGINT NOT NULL,
    tx_hash      TEXT,
    action       TEXT NOT NULL,
    caller       TEXT,
    receiver     TEXT,
    asset        TEXT,
    amount       NUMERIC,
    amount2      NUMERIC,
    fee          NUMERIC,
    lp           NUMERIC
);
CREATE INDEX IF NOT EXISTS clear_reserve_activity_reserve_block ON clear_reserve_activity (reserve, block_number);
CREATE INDEX IF NOT EXISTS clear_reserve_activity_action ON clear_reserve_activity (action);

-- IOU tokens (ERC20 clone per asset per reserve).
CREATE TABLE IF NOT EXISTS clear_iou_tokens (
    address      TEXT PRIMARY KEY,
    total_supply NUMERIC NOT NULL DEFAULT 0,
    last_block   BIGINT
);
CREATE TABLE IF NOT EXISTS clear_iou_balances (
    token      TEXT NOT NULL,
    holder     TEXT NOT NULL,
    balance    NUMERIC NOT NULL DEFAULT 0,
    last_block BIGINT,
    PRIMARY KEY (token, holder)
);
CREATE INDEX IF NOT EXISTS clear_iou_balances_holder ON clear_iou_balances (holder);

-- Curve StableSwap-NG pools (IOU secondary market; LP token IS the pool).
CREATE TABLE IF NOT EXISTS clear_curve_pools (
    address     TEXT PRIMARY KEY,
    lp_supply   NUMERIC NOT NULL DEFAULT 0,
    swap_count  BIGINT  NOT NULL DEFAULT 0,
    first_block BIGINT,
    last_block  BIGINT
);
CREATE TABLE IF NOT EXISTS clear_curve_lp_balances (
    pool       TEXT NOT NULL,
    holder     TEXT NOT NULL,
    balance    NUMERIC NOT NULL DEFAULT 0,
    last_block BIGINT,
    PRIMARY KEY (pool, holder)
);
CREATE INDEX IF NOT EXISTS clear_curve_lp_balances_holder ON clear_curve_lp_balances (holder);

-- Curve swap history (TokenExchange / TokenExchangeUnderlying).
CREATE TABLE IF NOT EXISTS clear_curve_swaps (
    id            TEXT PRIMARY KEY,
    pool          TEXT NOT NULL,
    block_number  BIGINT NOT NULL,
    log_index     BIGINT NOT NULL,
    tx_hash       TEXT,
    buyer         TEXT,
    sold_id       BIGINT,
    tokens_sold   NUMERIC,
    bought_id     BIGINT,
    tokens_bought NUMERIC,
    underlying    BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS clear_curve_swaps_pool_block ON clear_curve_swaps (pool, block_number);

-- Curve liquidity history (Add / Remove / RemoveOne / RemoveImbalance). The
-- token_amounts/fees arrays are stored as JSON (evmi serializes uint256[] as a
-- JSON array of decimal strings).
CREATE TABLE IF NOT EXISTS clear_curve_liquidity (
    id            TEXT PRIMARY KEY,
    pool          TEXT NOT NULL,
    block_number  BIGINT NOT NULL,
    log_index     BIGINT NOT NULL,
    tx_hash       TEXT,
    provider      TEXT,
    kind          TEXT NOT NULL,
    token_amounts JSONB,
    fees          JSONB,
    token_id      BIGINT,
    token_amount  NUMERIC,
    coin_amount   NUMERIC,
    invariant     NUMERIC,
    token_supply  NUMERIC
);
CREATE INDEX IF NOT EXISTS clear_curve_liquidity_pool_block ON clear_curve_liquidity (pool, block_number);
`
