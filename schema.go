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
-- 'position' is the asset's index in the reserve's append-only assetList (== the
-- AssetAdded emission order), used to map a Deposit/Withdraw amounts[i] back to
-- its token.
CREATE TABLE IF NOT EXISTS clear_reserve_assets (
    reserve    TEXT NOT NULL,
    asset      TEXT NOT NULL,
    decimals   INT,
    iou        TEXT,
    position   INT,
    iou_supply NUMERIC NOT NULL DEFAULT 0,
    PRIMARY KEY (reserve, asset)
);
-- Idempotent migrations for reserves indexed before these columns existed.
ALTER TABLE clear_reserve_assets ADD COLUMN IF NOT EXISTS position INT;
ALTER TABLE clear_reserve_assets ADD COLUMN IF NOT EXISTS iou_supply NUMERIC NOT NULL DEFAULT 0;

-- Reserve's physical ERC20 holdings, one row per underlying asset, reconstructed
-- from the token flows in deposit/withdraw/single-asset/rebalance/swap/IOU/flash
-- events (each moves real tokens in or out of the reserve). BASE reserves only;
-- exact only when indexing starts at the reserve's deployment (so no flow and no
-- AssetAdded is missed). This is the true on-chain balanceOf(reserve), unlike the
-- LP/NAV figures in clear_reserves.
CREATE TABLE IF NOT EXISTS clear_reserve_token_balances (
    reserve    TEXT NOT NULL,
    asset      TEXT NOT NULL,
    balance    NUMERIC NOT NULL DEFAULT 0,
    last_block BIGINT,
    PRIMARY KEY (reserve, asset)
);
CREATE INDEX IF NOT EXISTS clear_reserve_token_balances_asset ON clear_reserve_token_balances (asset);

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

-- Oracle state, one row per asset. Config comes from OracleConfigured; price from
-- ClearOracleRateChanged; redemption_price from ClearOracleRedemptionPriceChanged
-- (all on the ClearOracle contract) plus the PythOracleAdapter's PriceUpdated.
-- last_refresh is the block-header timestamp (unix seconds) of the most recent
-- oracle event — the "last refresh date". last_block is the same event's block
-- number. Keyed by asset so both contracts fold into the same row.
CREATE TABLE IF NOT EXISTS clear_oracle_prices (
    asset            TEXT PRIMARY KEY,
    oracle           TEXT,
    enabled          BOOLEAN,
    asset_decimals   INT,
    oracle_decimals  INT,
    price_ttl        NUMERIC,
    price            NUMERIC,
    redemption_price NUMERIC,
    last_refresh     BIGINT,
    last_block       BIGINT
);

-- Oracle price time series: one row per ClearOracleRateChanged (every price write,
-- including Pyth-driven ones, funnels through it). Append-only, for charting price
-- history. block_timestamp is the block-header unix seconds.
CREATE TABLE IF NOT EXISTS clear_oracle_price_history (
    id              TEXT PRIMARY KEY,
    asset           TEXT NOT NULL,
    block_number    BIGINT NOT NULL,
    block_timestamp BIGINT,
    price           NUMERIC
);
CREATE INDEX IF NOT EXISTS clear_oracle_price_history_asset ON clear_oracle_price_history (asset, block_number);

-- Reserve value time series (BASE reserves), DAILY granularity: one row per reserve
-- per UTC day (day = the block-timestamp's date). total_assets = Σ balance*10^(18-
-- decimals) (par-valued gross holdings, 18-dec — the contract's totalAssets()) and
-- total_supply (LP supply). Each row holds the LAST (highest-block) snapshot of that
-- day, so it's the end-of-day value. For charting TVL / supply over time.
CREATE TABLE IF NOT EXISTS clear_reserve_value_history (
    reserve         TEXT NOT NULL,
    day             DATE NOT NULL,
    block_number    BIGINT,
    block_timestamp BIGINT,
    total_assets    NUMERIC,
    total_supply    NUMERIC,
    PRIMARY KEY (reserve, day)
);

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
