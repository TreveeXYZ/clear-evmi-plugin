package main

// schema is the full set of tables the exporter writes. Applied on Init when
// autoMigrate is enabled. All amounts are NUMERIC (uint256-safe); addresses are
// stored lowercased. `id` columns are evmi's stable "chainId:block:logIndex".
//
// Every table carries a chain_id column so one database can hold state for more
// than one chain. For entity/address-keyed tables (reserves, balances, asset and
// contract registries, oracle prices, curve pools, daily value history) chain_id
// is part of the PRIMARY KEY — the same address can exist on two chains (e.g. a
// deterministic CREATE2 deployment) and must not collide. History tables keyed by
// the event `id` are already globally unique (id embeds the chain id), so chain_id
// is a plain column there for filtering.
//
// NOTE: adding chain_id changes the primary keys of existing tables. CREATE TABLE
// IF NOT EXISTS will NOT alter a table that predates this change — an existing
// deployment needs a real migration (or a drop of the clear_* tables on a scratch
// DB, as the integration test does).
const schema = `
-- Idempotency: at-least-once delivery means a log may arrive twice; we record
-- every processed event id and skip repeats, so balance math stays exact. The id
-- already embeds the chain id, so it stays the sole primary key; chain_id is kept
-- as a column for per-chain filtering.
CREATE TABLE IF NOT EXISTS clear_processed_events (
    id       TEXT PRIMARY KEY,
    chain_id BIGINT NOT NULL
);

-- Tracked-contracts registry (address -> kind), the source of truth for routing.
-- The exporter looks up a log's contract by ADDRESS here instead of re-classifying
-- its ABI name on every event. Seeded from pluginConfig.contracts on Init
-- (source='config') and extended at runtime whenever a new contract is detected —
-- a reserve-spawned IOU at AssetAdded, a reserve announced by the
-- ClearReserveFactory (NewClearReserve), a Curve pool announced by the
-- ClearCurvePoolDeployer (PoolDeployed), or any address seen for the first time
-- (source='dynamic'). Reloaded into the in-memory registry on start so discovery
-- survives restarts. kind is one of: base_reserve, meta_reserve, iou, curve,
-- oracle, factory, curve_deployer.
CREATE TABLE IF NOT EXISTS clear_contracts (
    chain_id    BIGINT NOT NULL,
    address     TEXT NOT NULL,
    kind        TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'config',
    first_block BIGINT,
    PRIMARY KEY (chain_id, address)
);
CREATE INDEX IF NOT EXISTS clear_contracts_kind ON clear_contracts (chain_id, kind);

-- Reserves (Clear base/meta reserves; the LP token IS the reserve contract).
-- name/symbol/implementation/reserve_index/tokens/factory come from the factory's
-- NewClearReserve event and are NULL for a reserve indexed from its own logs only
-- (i.e. when indexing starts after its deployment, or the factory isn't a source).
CREATE TABLE IF NOT EXISTS clear_reserves (
    chain_id          BIGINT NOT NULL,
    address           TEXT NOT NULL,
    kind              TEXT NOT NULL,
    lp_supply         NUMERIC NOT NULL DEFAULT 0,
    total_deposits    NUMERIC NOT NULL DEFAULT 0,
    total_withdrawals NUMERIC NOT NULL DEFAULT 0,
    iou_minted        NUMERIC NOT NULL DEFAULT 0,
    iou_redeemed      NUMERIC NOT NULL DEFAULT 0,
    swap_count        BIGINT  NOT NULL DEFAULT 0,
    name              TEXT,
    symbol            TEXT,
    implementation    TEXT,
    reserve_index     BIGINT,
    tokens            JSONB,
    factory           TEXT,
    first_block       BIGINT,
    last_block        BIGINT,
    PRIMARY KEY (chain_id, address)
);
-- Idempotent migrations for databases created before NewClearReserve was indexed.
ALTER TABLE clear_reserves ADD COLUMN IF NOT EXISTS name TEXT;
ALTER TABLE clear_reserves ADD COLUMN IF NOT EXISTS symbol TEXT;
ALTER TABLE clear_reserves ADD COLUMN IF NOT EXISTS implementation TEXT;
ALTER TABLE clear_reserves ADD COLUMN IF NOT EXISTS reserve_index BIGINT;
ALTER TABLE clear_reserves ADD COLUMN IF NOT EXISTS tokens JSONB;
ALTER TABLE clear_reserves ADD COLUMN IF NOT EXISTS factory TEXT;

-- Reserve governance parameters, one row per reserve, folded from the set* events
-- (ConfigUpdated, FlashFeeUpdated, BaseQuoteFeeBpsUpdated, SingleAssetFeeBpsUpdated,
-- RedemptionProximityBpsUpdated, RebalanceTriggerBpsUpdated,
-- DepositWeightToleranceBpsUpdated, IouDistributionUpdated,
-- BaseQuoteDistributionUpdated, SwapSpreadBpsUpdated). Each event patches only its
-- own column(s), so a column is NULL until its parameter is first set — the
-- contract defaults (see RESERVE-SETTINGS.md) are NOT emitted at initialize and so
-- cannot be observed from events. rebalance_trigger_bps is base-only,
-- deposit_weight_tolerance_bps meta-only.
CREATE TABLE IF NOT EXISTS clear_reserve_settings (
    chain_id                     BIGINT NOT NULL,
    reserve                      TEXT NOT NULL,
    kind                         TEXT NOT NULL,
    config_address               TEXT,
    flash_fee_bps                NUMERIC,
    base_quote_fee_bps           NUMERIC,
    single_asset_fee_bps         NUMERIC,
    redemption_proximity_bps     NUMERIC,
    rebalance_trigger_bps        NUMERIC,
    deposit_weight_tolerance_bps NUMERIC,
    iou_trader_bps               NUMERIC,
    iou_treasury_bps             NUMERIC,
    base_quote_trader_bps        NUMERIC,
    base_quote_treasury_bps      NUMERIC,
    swap_spread_min_bps          NUMERIC,
    swap_spread_max_bps          NUMERIC,
    last_block                   BIGINT,
    PRIMARY KEY (chain_id, reserve)
);

-- Protocol-wide config held by the ClearReserveFactory (which IS the
-- IClearReserveConfig every reserve points at): the treasury (NewTreasury) and the
-- current implementation of each clone type with the version the factory bumped it
-- to (New{BaseReserve,MetaReserve,IOU}Implementation). One row per factory.
CREATE TABLE IF NOT EXISTS clear_protocol_config (
    chain_id                    BIGINT NOT NULL,
    factory                     TEXT NOT NULL,
    treasury                    TEXT,
    base_reserve_implementation TEXT,
    base_reserve_version        NUMERIC,
    meta_reserve_implementation TEXT,
    meta_reserve_version        NUMERIC,
    iou_implementation          TEXT,
    iou_version                 NUMERIC,
    last_block                  BIGINT,
    PRIMARY KEY (chain_id, factory)
);

CREATE TABLE IF NOT EXISTS clear_reserve_lp_balances (
    chain_id   BIGINT NOT NULL,
    reserve    TEXT NOT NULL,
    holder     TEXT NOT NULL,
    balance    NUMERIC NOT NULL DEFAULT 0,
    last_block BIGINT,
    PRIMARY KEY (chain_id, reserve, holder)
);
CREATE INDEX IF NOT EXISTS clear_reserve_lp_balances_holder ON clear_reserve_lp_balances (chain_id, holder);

-- Per-reserve asset registry (from AssetAdded), including the asset's IOU token.
-- 'position' is the asset's index in the reserve's append-only assetList (== the
-- AssetAdded emission order), used to map a Deposit/Withdraw amounts[i] back to
-- its token. Both reserve types populate this: a BASE reserve emits AssetAdded once
-- per underlying asset, a META reserve once per leg (native, then BaseLP).
-- 'weight' is the leg's target weight in bps (int256) and is META-only — a base
-- reserve values its assets at par and has no target weights, so it stays NULL.
CREATE TABLE IF NOT EXISTS clear_reserve_assets (
    chain_id   BIGINT NOT NULL,
    reserve    TEXT NOT NULL,
    asset      TEXT NOT NULL,
    decimals   INT,
    iou        TEXT,
    position   INT,
    iou_supply NUMERIC NOT NULL DEFAULT 0,
    weight     NUMERIC,
    PRIMARY KEY (chain_id, reserve, asset)
);
-- Idempotent migrations for reserves indexed before these columns existed.
ALTER TABLE clear_reserve_assets ADD COLUMN IF NOT EXISTS position INT;
ALTER TABLE clear_reserve_assets ADD COLUMN IF NOT EXISTS iou_supply NUMERIC NOT NULL DEFAULT 0;
ALTER TABLE clear_reserve_assets ADD COLUMN IF NOT EXISTS weight NUMERIC;

-- Reserve's physical ERC20 holdings, one row per underlying asset, reconstructed
-- from the token flows in deposit/withdraw/single-asset/rebalance/swap/IOU/flash
-- events (each moves real tokens in or out of the reserve). BASE reserves only;
-- exact only when indexing starts at the reserve's deployment (so no flow and no
-- AssetAdded is missed). This is the true on-chain balanceOf(reserve), unlike the
-- LP/NAV figures in clear_reserves.
CREATE TABLE IF NOT EXISTS clear_reserve_token_balances (
    chain_id   BIGINT NOT NULL,
    reserve    TEXT NOT NULL,
    asset      TEXT NOT NULL,
    balance    NUMERIC NOT NULL DEFAULT 0,
    last_block BIGINT,
    PRIMARY KEY (chain_id, reserve, asset)
);
CREATE INDEX IF NOT EXISTS clear_reserve_token_balances_asset ON clear_reserve_token_balances (chain_id, asset);

-- Depeg-swap history (the Swap event).
CREATE TABLE IF NOT EXISTS clear_reserve_swaps (
    id           TEXT PRIMARY KEY,
    chain_id     BIGINT NOT NULL,
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
CREATE INDEX IF NOT EXISTS clear_reserve_swaps_reserve_block ON clear_reserve_swaps (chain_id, reserve, block_number);

-- Everything else that happens on a reserve (deposits, withdrawals, rebalances,
-- IOU mint/redeem, flash loans) as a unified activity log.
CREATE TABLE IF NOT EXISTS clear_reserve_activity (
    id           TEXT PRIMARY KEY,
    chain_id     BIGINT NOT NULL,
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
CREATE INDEX IF NOT EXISTS clear_reserve_activity_reserve_block ON clear_reserve_activity (chain_id, reserve, block_number);
CREATE INDEX IF NOT EXISTS clear_reserve_activity_action ON clear_reserve_activity (chain_id, action);

-- IOU tokens (ERC20 clone per asset per reserve). total_supply is derived from the
-- zero-address Transfer decomposition; treasury_fees is the cumulative treasury cut
-- of every mint, which only ClearIOUMinted carries (the Transfers show the combined
-- movement, not the split).
CREATE TABLE IF NOT EXISTS clear_iou_tokens (
    chain_id      BIGINT NOT NULL,
    address       TEXT NOT NULL,
    total_supply  NUMERIC NOT NULL DEFAULT 0,
    treasury_fees NUMERIC NOT NULL DEFAULT 0,
    last_block    BIGINT,
    PRIMARY KEY (chain_id, address)
);
ALTER TABLE clear_iou_tokens ADD COLUMN IF NOT EXISTS treasury_fees NUMERIC NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS clear_iou_balances (
    chain_id   BIGINT NOT NULL,
    token      TEXT NOT NULL,
    holder     TEXT NOT NULL,
    balance    NUMERIC NOT NULL DEFAULT 0,
    last_block BIGINT,
    PRIMARY KEY (chain_id, token, holder)
);
CREATE INDEX IF NOT EXISTS clear_iou_balances_holder ON clear_iou_balances (chain_id, holder);

-- Oracle state, one row per asset. Config comes from OracleConfigured; price from
-- ClearOracleRateChanged; redemption_price from ClearOracleRedemptionPriceChanged
-- (all on the ClearOracle contract) plus the PythOracleAdapter's PriceUpdated.
-- last_refresh is the block-header timestamp (unix seconds) of the most recent
-- oracle event — the "last refresh date". last_block is the same event's block
-- number. publish_time is Pyth's own timestamp for the price (PriceUpdated), i.e.
-- when the feed published it rather than when it landed on-chain. Keyed by
-- (chain_id, asset) so both contracts fold into the same row.
CREATE TABLE IF NOT EXISTS clear_oracle_prices (
    chain_id         BIGINT NOT NULL,
    asset            TEXT NOT NULL,
    oracle           TEXT,
    enabled          BOOLEAN,
    asset_decimals   INT,
    oracle_decimals  INT,
    price_ttl        NUMERIC,
    price            NUMERIC,
    redemption_price NUMERIC,
    publish_time     BIGINT,
    last_refresh     BIGINT,
    last_block       BIGINT,
    PRIMARY KEY (chain_id, asset)
);
ALTER TABLE clear_oracle_prices ADD COLUMN IF NOT EXISTS publish_time BIGINT;

-- Oracle price time series: one row per ClearOracleRateChanged (every price write,
-- including Pyth-driven ones, funnels through it). Append-only, for charting price
-- history. block_timestamp is the block-header unix seconds.
CREATE TABLE IF NOT EXISTS clear_oracle_price_history (
    id              TEXT PRIMARY KEY,
    chain_id        BIGINT NOT NULL,
    asset           TEXT NOT NULL,
    block_number    BIGINT NOT NULL,
    block_timestamp BIGINT,
    price           NUMERIC
);
CREATE INDEX IF NOT EXISTS clear_oracle_price_history_asset ON clear_oracle_price_history (chain_id, asset, block_number);

-- Reserve value time series (BASE reserves), DAILY granularity: one row per reserve
-- per UTC day (day = the block-timestamp's date). total_assets = Σ balance*10^(18-
-- decimals) (par-valued gross holdings, 18-dec — the contract's totalAssets()) and
-- total_supply (LP supply). Each row holds the LAST (highest-block) snapshot of that
-- day, so it's the end-of-day value. For charting TVL / supply over time.
CREATE TABLE IF NOT EXISTS clear_reserve_value_history (
    chain_id        BIGINT NOT NULL,
    reserve         TEXT NOT NULL,
    day             DATE NOT NULL,
    block_number    BIGINT,
    block_timestamp BIGINT,
    total_assets    NUMERIC,
    total_supply    NUMERIC,
    PRIMARY KEY (chain_id, reserve, day)
);

-- Curve StableSwap-NG pools (IOU secondary market; LP token IS the pool).
-- reserve/base_pool/coin/is_base_pool/deployer come from the ClearCurvePoolDeployer's
-- PoolDeployed event and stay NULL for a pool indexed from its own logs only.
-- is_base_pool marks the reserve's plain base pool (PoolDeployed carries coin = 0
-- and pool == base_pool there); otherwise the pool is a metapool pairing `coin` —
-- an asset's IOU, or a meta reserve's native token / native IOU — against base_pool.
CREATE TABLE IF NOT EXISTS clear_curve_pools (
    chain_id     BIGINT NOT NULL,
    address      TEXT NOT NULL,
    lp_supply    NUMERIC NOT NULL DEFAULT 0,
    swap_count   BIGINT  NOT NULL DEFAULT 0,
    reserve      TEXT,
    base_pool    TEXT,
    coin         TEXT,
    is_base_pool BOOLEAN,
    deployer     TEXT,
    first_block  BIGINT,
    last_block   BIGINT,
    PRIMARY KEY (chain_id, address)
);
ALTER TABLE clear_curve_pools ADD COLUMN IF NOT EXISTS reserve TEXT;
ALTER TABLE clear_curve_pools ADD COLUMN IF NOT EXISTS base_pool TEXT;
ALTER TABLE clear_curve_pools ADD COLUMN IF NOT EXISTS coin TEXT;
ALTER TABLE clear_curve_pools ADD COLUMN IF NOT EXISTS is_base_pool BOOLEAN;
ALTER TABLE clear_curve_pools ADD COLUMN IF NOT EXISTS deployer TEXT;
CREATE INDEX IF NOT EXISTS clear_curve_pools_reserve ON clear_curve_pools (chain_id, reserve);
CREATE TABLE IF NOT EXISTS clear_curve_lp_balances (
    chain_id   BIGINT NOT NULL,
    pool       TEXT NOT NULL,
    holder     TEXT NOT NULL,
    balance    NUMERIC NOT NULL DEFAULT 0,
    last_block BIGINT,
    PRIMARY KEY (chain_id, pool, holder)
);
CREATE INDEX IF NOT EXISTS clear_curve_lp_balances_holder ON clear_curve_lp_balances (chain_id, holder);

-- Curve swap history (TokenExchange / TokenExchangeUnderlying).
CREATE TABLE IF NOT EXISTS clear_curve_swaps (
    id            TEXT PRIMARY KEY,
    chain_id      BIGINT NOT NULL,
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
CREATE INDEX IF NOT EXISTS clear_curve_swaps_pool_block ON clear_curve_swaps (chain_id, pool, block_number);

-- Curve liquidity history (Add / Remove / RemoveOne / RemoveImbalance). The
-- token_amounts/fees arrays are stored as JSON (evmi serializes uint256[] as a
-- JSON array of decimal strings).
CREATE TABLE IF NOT EXISTS clear_curve_liquidity (
    id            TEXT PRIMARY KEY,
    chain_id      BIGINT NOT NULL,
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
CREATE INDEX IF NOT EXISTS clear_curve_liquidity_pool_block ON clear_curve_liquidity (chain_id, pool, block_number);
`
