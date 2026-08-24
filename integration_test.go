package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	exporter "github.com/evmi-cloud/go-evm-indexer/pkg/exporter"
)

// Integration test against a real PostgreSQL (the plugin uses NUMERIC / JSONB /
// GREATEST / ON CONFLICT, so SQLite won't do). Gated behind CLEAR_DEFI_POSTGRES_DSN,
// e.g.:
//
//	CLEAR_DEFI_POSTGRES_DSN='postgres://evmi:evmi@localhost:5432/evmi?sslmode=disable' \
//	  go test ./examples/exporters/clear-defi/ -run TestReplayProtocol -v
//
// It drops the clear_* tables first, so point it at a scratch database.

const (
	zeroAddr = "0x0000000000000000000000000000000000000000"
	reserve  = "0x00000000000000000000000000000000re5ervea"
	usdc     = "0x00000000000000000000000000000000usdc0000"
	usdt     = "0x00000000000000000000000000000000usdt0000"
	iou1     = "0x0000000000000000000000000000000000iou001"
	iou2     = "0x0000000000000000000000000000000000iou002"
	pool     = "0x00000000000000000000000000000000poo10001"
	alice    = "0x000000000000000000000000000000000a11ce00"
	bob      = "0x00000000000000000000000000000000000b0b000"
	carol    = "0x0000000000000000000000000000000000car01d0"
	dave     = "0x00000000000000000000000000000000000dave00"
	eve      = "0x0000000000000000000000000000000000000eve0"

	// Discovery contracts and the entities they announce.
	factory      = "0x00000000000000000000000000000000fac70201"
	deployer     = "0x000000000000000000000000000000000dep10ee"
	metaReserve  = "0x0000000000000000000000000000000me7are5e0"
	nativeTok    = "0x00000000000000000000000000000000na7ive00"
	metaIou1     = "0x00000000000000000000000000000000me7aiou1"
	metaIou2     = "0x00000000000000000000000000000000me7aiou2"
	iouPool      = "0x00000000000000000000000000000000ioupoo10"
	treasuryAddr = "0x0000000000000000000000000000000007rea5u0"
	implAddr     = "0x0000000000000000000000000000000000imp100"
)

var allTables = []string{
	"clear_processed_events", "clear_contracts", "clear_reserves", "clear_reserve_lp_balances",
	"clear_reserve_assets", "clear_reserve_token_balances", "clear_reserve_swaps",
	"clear_reserve_activity", "clear_reserve_value_history", "clear_reserve_settings",
	"clear_protocol_config", "clear_iou_tokens",
	"clear_iou_balances", "clear_oracle_prices", "clear_oracle_price_history",
	"clear_curve_pools", "clear_curve_lp_balances", "clear_curve_swaps",
	"clear_curve_liquidity",
}

// blockTs is the synthetic block-header timestamp for a block (unix seconds).
func blockTs(block uint64) uint64 { return 1_700_000_000 + block }

func mkLog(block, idx uint64, contract, addr, event string, args map[string]string) exporter.LogEvent {
	return exporter.LogEvent{
		Id:              fmt.Sprintf("1:%d:%d", block, idx),
		ChainId:         1,
		ContractName:    contract,
		EventName:       event,
		Address:         addr,
		Args:            args,
		BlockNumber:     block,
		BlockTimestamp:  blockTs(block),
		LogIndex:        idx,
		TransactionHash: fmt.Sprintf("0xtx%d%d", block, idx),
	}
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// eq asserts exactly one row matches; used to check NUMERIC columns by passing the
// expected decimal string as a param (PostgreSQL casts text -> numeric).
func eq(t *testing.T, db *sql.DB, label, q string, args ...any) {
	t.Helper()
	if n := count(t, db, q, args...); n != 1 {
		t.Errorf("%s: expected exactly 1 matching row, got %d", label, n)
	}
}

func TestReplayProtocol(t *testing.T) {
	dsn := os.Getenv("CLEAR_DEFI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set CLEAR_DEFI_POSTGRES_DSN to run the clear-defi integration test")
	}

	// Clean slate.
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, tbl := range allTables {
		if _, err := raw.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE"); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	raw.Close()

	const R = "ClearBaseReserve"
	const META = "ClearMetaReserve"
	const IOU = "ClearIOU"
	const CURVE = "CurveStableSwapNG"
	const ORACLE = "ClearOracle"
	const ADAPTER = "PythOracleAdapter"
	const FACTORY = "ClearReserveFactory"
	const DEPLOYER = "ClearCurvePoolDeployer"
	const oracleAddr = "0x0000000000000000000000000000000000rac1e"
	const adapterAddr = "0x00000000000000000000000000000000adap7e0"

	e := &clearExporter{}
	// Seed the address->kind registry from config (the base reserve, pool, both
	// oracle contracts, the reserve factory and the curve pool deployer). IOUs are
	// discovered at AssetAdded, reserves at NewClearReserve, deployer-built pools at
	// PoolDeployed — none of those need to be listed here.
	cfg, _ := json.Marshal(pluginConfig{
		Dsn: dsn,
		Contracts: []contractConfig{
			{Address: reserve, Kind: "base_reserve"},
			{Address: pool, Kind: "curve"},
			{Address: oracleAddr, Kind: "oracle"},
			{Address: adapterAddr, Kind: "oracle"},
			{Address: factory, Kind: "factory"},
			{Address: deployer, Kind: "curve_deployer"},
		},
	})
	if err := e.Init(exporter.Context{ExporterName: "clear-defi-test", PipelineId: 1, ChainId: 1, Config: cfg}); err != nil {
		t.Fatalf("init: %v", err)
	}
	defer e.Close()
	db := e.db

	// Config contracts are recorded in the registry per chain on Init.
	eq(t, db, "reserve contract", `SELECT count(*) FROM clear_contracts WHERE chain_id=1 AND address=$1 AND kind='base_reserve' AND source='config'`, normAddr(reserve))
	eq(t, db, "curve contract", `SELECT count(*) FROM clear_contracts WHERE chain_id=1 AND address=$1 AND kind='curve' AND source='config'`, normAddr(pool))
	eq(t, db, "oracle contract", `SELECT count(*) FROM clear_contracts WHERE chain_id=1 AND address=$1 AND kind='oracle' AND source='config'`, normAddr(oracleAddr))

	// A realistic session. Amounts chosen so balances/supply are easy to verify.
	// The reserve holds two assets (usdc pos 0, usdt pos 1); Deposit/Withdraw carry
	// a per-asset `amounts` array aligned with that order, so the reserve's physical
	// ERC20 holdings (clear_reserve_token_balances) can be reconstructed exactly.
	logs := []exporter.LogEvent{
		// Reserve setup: register both assets so amounts[] maps to positions 0/1.
		mkLog(100, 0, R, reserve, "AssetAdded", map[string]string{"asset": usdc, "decimals": "6", "iou": iou1}),
		mkLog(100, 1, R, reserve, "AssetAdded", map[string]string{"asset": usdt, "decimals": "6", "iou": iou2}),
		// Alice deposit: 1000 LP, 1000 usdc + 1000 usdt in.
		mkLog(100, 2, R, reserve, "Transfer", map[string]string{"from": zeroAddr, "to": alice, "value": "1000"}),
		mkLog(100, 3, R, reserve, "Deposit", map[string]string{"caller": alice, "receiver": alice, "lpMinted": "1000", "amounts": `["1000","1000"]`}),
		// Bob deposit: 500 LP, 500 usdc + 500 usdt in.
		mkLog(101, 0, R, reserve, "Transfer", map[string]string{"from": zeroAddr, "to": bob, "value": "500"}),
		// fmt.Sprint rendering of amounts[]; the JSON form is exercised above.
		mkLog(101, 1, R, reserve, "Deposit", map[string]string{"caller": bob, "receiver": bob, "lpMinted": "500", "amounts": "[500 500]"}),
		// Alice sends 200 LP to Bob (no supply / token change).
		mkLog(102, 0, R, reserve, "Transfer", map[string]string{"from": alice, "to": bob, "value": "200"}),
		// Carol depeg swap: 100 usdc in, 95 usdt out, 5 IOU shortfall.
		mkLog(102, 1, R, reserve, "Swap", map[string]string{
			"trader": carol, "tokenIn": usdc, "tokenOut": usdt, "recipient": carol,
			"amountIn": "100", "amountOut": "95", "iouTotal": "5", "traderIOU": "3", "treasuryIOU": "1", "lpIOU": "1"}),
		// Carol separately mints 5 usdc-IOU (mintIOU): 5 usdc pulled in, 5 IOU minted.
		mkLog(102, 2, R, reserve, "IOUMinted", map[string]string{"caller": carol, "asset": usdc, "receiver": carol, "amount": "5"}),
		mkLog(102, 3, IOU, iou1, "Transfer", map[string]string{"from": zeroAddr, "to": carol, "value": "5"}),
		// Alice withdraws 100 LP: 100 usdc + 100 usdt out.
		mkLog(103, 0, R, reserve, "Transfer", map[string]string{"from": alice, "to": zeroAddr, "value": "100"}),
		mkLog(103, 1, R, reserve, "Withdraw", map[string]string{"caller": alice, "receiver": alice, "lpBurned": "100", "amounts": `["100","100"]`}),
		// Dave single-asset deposit: 50 usdc in, 1 usdc fee to treasury (net +49), 50 LP.
		mkLog(103, 2, R, reserve, "Transfer", map[string]string{"from": zeroAddr, "to": dave, "value": "50"}),
		mkLog(103, 3, R, reserve, "SingleAssetDeposit", map[string]string{"caller": dave, "receiver": dave, "asset": usdc, "amountIn": "50", "fee": "1", "lpMinted": "50"}),
		// Permissionless rebalance: 30 usdt in, 29 usdc out.
		mkLog(103, 4, R, reserve, "Rebalanced", map[string]string{"caller": bob, "tokenIn": usdt, "tokenOut": usdc, "recipient": bob, "amountIn": "30", "amountOut": "29"}),

		// Curve pool: Dave adds liquidity, Eve swaps, Dave removes one-sided.
		// Curve (Vyper) uses sender/receiver on Transfer.
		mkLog(104, 0, CURVE, pool, "Transfer", map[string]string{"sender": zeroAddr, "receiver": dave, "value": "1000"}),
		mkLog(104, 1, CURVE, pool, "AddLiquidity", map[string]string{
			"provider": dave, "token_amounts": "[500 500]", "fees": "[0 0]", "invariant": "1000", "token_supply": "1000"}),
		mkLog(104, 2, CURVE, pool, "TokenExchange", map[string]string{
			"buyer": eve, "sold_id": "0", "tokens_sold": "100", "bought_id": "1", "tokens_bought": "99"}),
		mkLog(105, 0, CURVE, pool, "Transfer", map[string]string{"sender": dave, "receiver": zeroAddr, "value": "200"}),
		mkLog(105, 1, CURVE, pool, "RemoveLiquidityOne", map[string]string{
			"provider": dave, "token_id": "0", "token_amount": "200", "coin_amount": "200", "token_supply": "800"}),

		// Oracle: configure usdc, push a price, set redemption, then a Pyth refresh
		// (PriceUpdated on the adapter carries the publishTime = last refresh date).
		mkLog(106, 0, ORACLE, oracleAddr, "OracleConfigured", map[string]string{
			"enabled": "true", "asset": usdc, "assetDecimals": "6", "oracleDecimals": "8",
			"priceTTL": "3600", "redemptionPrice": "100000000"}),
		mkLog(106, 1, ORACLE, oracleAddr, "ClearOracleRateChanged", map[string]string{"asset": usdc, "price": "99990000"}),
		mkLog(106, 2, ORACLE, oracleAddr, "ClearOracleRedemptionPriceChanged", map[string]string{"asset": usdc, "redemptionPrice": "100000000"}),
		mkLog(107, 0, ADAPTER, adapterAddr, "PriceUpdated", map[string]string{"asset": usdc, "price": "99990000", "publishTime": "1721600000"}),

		// Governance settings on the base reserve: each event patches only its own
		// column(s) of the reserve's clear_reserve_settings row.
		mkLog(108, 0, R, reserve, "SingleAssetFeeBpsUpdated", map[string]string{"singleAssetFeeBps": "7"}),
		mkLog(108, 1, R, reserve, "SwapSpreadBpsUpdated", map[string]string{"minBps": "3", "maxBps": "500"}),
		mkLog(108, 2, R, reserve, "IouDistributionUpdated", map[string]string{"traderBps": "2000", "treasuryBps": "4000"}),
		mkLog(108, 3, R, reserve, "RebalanceTriggerBpsUpdated", map[string]string{"triggerBps": "7000"}),
		// The IOU's own mint event carries the treasury's cut, which the paired
		// Transfers do not (they only show the combined movement).
		mkLog(108, 4, IOU, iou1, "ClearIOUMinted", map[string]string{"to": carol, "amount": "5", "treasuryFee": "1"}),

		// Factory: protocol config, then a meta reserve deployed. The meta reserve is
		// registered from NewClearReserve alone — it is NOT in pluginConfig.contracts.
		mkLog(109, 0, FACTORY, factory, "NewTreasury", map[string]string{"previousTreasury": zeroAddr, "newTreasury": treasuryAddr}),
		mkLog(109, 1, FACTORY, factory, "NewMetaReserveImplementation", map[string]string{"version": "2", "implementation": implAddr}),
		mkLog(109, 2, FACTORY, factory, "NewClearReserve", map[string]string{
			"index": "1", "implementation": implAddr, "reserveType": "1", "reserve": metaReserve,
			// tokens[] in Go's fmt.Sprint rendering — what a server whose formatArgValue
			// has no slice case emits. It is NOT JSON; it must still land in the JSONB
			// column as a proper array (this is what broke live).
			"name": "Clear Meta USD", "symbol": "cmUSD", "tokens": "[" + reserve + " " + nativeTok + "]"}),
		// Meta AssetAdded: one per leg (native first, then BaseLP), each with the leg's
		// IOU and its target weight in bps.
		mkLog(109, 3, META, metaReserve, "AssetAdded", map[string]string{"asset": nativeTok, "decimals": "6", "iou": metaIou1, "weight": "2000"}),
		mkLog(109, 4, META, metaReserve, "AssetAdded", map[string]string{"asset": reserve, "decimals": "18", "iou": metaIou2, "weight": "8000"}),

		// Curve pool deployer: the reserve's plain base pool (coin = 0), then an IOU
		// metapool against it. Both are registered as curve pools from these logs.
		mkLog(110, 0, DEPLOYER, deployer, "PoolDeployed", map[string]string{
			"reserve": reserve, "basePool": pool, "coin": zeroAddr, "pool": pool}),
		mkLog(110, 1, DEPLOYER, deployer, "PoolDeployed", map[string]string{
			"reserve": reserve, "basePool": pool, "coin": iou1, "pool": iouPool}),
	}

	for _, l := range logs {
		if err := e.NewLogEvent(l); err != nil {
			t.Fatalf("NewLogEvent %s: %v", l.Id, err)
		}
	}

	// --- reserve state ---
	// supply = 1000 + 500 - 100(burn) + 50(single) = 1450; deposits = 1550;
	// withdrawals = 100; iou_minted = 5; 1 swap.
	eq(t, db, "reserve state",
		`SELECT count(*) FROM clear_reserves WHERE address=$1 AND kind='base'
		 AND lp_supply=$2 AND total_deposits=$3 AND total_withdrawals=$4
		 AND iou_minted=$5 AND iou_redeemed=0 AND swap_count=1`,
		reserve, "1450", "1550", "100", "5")

	// balances: Alice = 1000 - 200 - 100 = 700; Bob = 500 + 200 = 700; Dave = 50.
	eq(t, db, "alice LP", `SELECT count(*) FROM clear_reserve_lp_balances WHERE reserve=$1 AND holder=$2 AND balance=$3`, reserve, alice, "700")
	eq(t, db, "bob LP", `SELECT count(*) FROM clear_reserve_lp_balances WHERE reserve=$1 AND holder=$2 AND balance=$3`, reserve, bob, "700")
	eq(t, db, "dave LP", `SELECT count(*) FROM clear_reserve_lp_balances WHERE reserve=$1 AND holder=$2 AND balance=$3`, reserve, dave, "50")
	if got := count(t, db, `SELECT COALESCE(sum(balance),0) FROM clear_reserve_lp_balances WHERE reserve=$1`, reserve); got != 1450 {
		t.Errorf("sum of LP balances = %d, want 1450 (== supply)", got)
	}

	// physical ERC20 holdings (reconstructed from token flows):
	//   usdc = +1000 +500 +100(swap in) +5(iou mint) -100(withdraw) +49(single net) -29(rebalance out) = 1525
	//   usdt = +1000 +500 -95(swap out) -100(withdraw) +30(rebalance in)                                = 1335
	eq(t, db, "usdc holdings", `SELECT count(*) FROM clear_reserve_token_balances WHERE reserve=$1 AND asset=$2 AND balance=$3`, reserve, usdc, "1525")
	eq(t, db, "usdt holdings", `SELECT count(*) FROM clear_reserve_token_balances WHERE reserve=$1 AND asset=$2 AND balance=$3`, reserve, usdt, "1335")

	// value history is DAILY: all reserve events fall on one UTC day, so the 7 flow
	// events collapse into a single row holding the end-of-day (last, block 103)
	// value. total_assets is par-valued 18-dec: (1525 + 1335) 6-dec * 10^12 = 2.86e15;
	// total_supply 1450.
	eq(t, db, "reserve daily value", `SELECT count(*) FROM clear_reserve_value_history
		WHERE reserve=$1 AND day=(to_timestamp($2) AT TIME ZONE 'UTC')::date AND block_number=103
		AND total_assets=2860000000000000 AND total_supply=1450`, reserve, blockTs(103))
	if got := count(t, db, `SELECT count(*) FROM clear_reserve_value_history WHERE reserve=$1`, reserve); got != 1 {
		t.Errorf("reserve daily rows = %d, want 1 (all events same day)", got)
	}

	// asset registry + IOU; position mirrors assetList order and drives amounts[] mapping.
	// usdc's IOU (iou1) minted 5 to Carol; usdt's IOU (iou2) is untouched → 0. The
	// iou_supply column mirrors clear_iou_tokens.total_supply for the linked IOU.
	eq(t, db, "asset usdc", `SELECT count(*) FROM clear_reserve_assets WHERE reserve=$1 AND asset=$2 AND iou=$3 AND decimals=6 AND position=0 AND iou_supply=5`, reserve, usdc, iou1)
	eq(t, db, "asset usdt", `SELECT count(*) FROM clear_reserve_assets WHERE reserve=$1 AND asset=$2 AND iou=$3 AND decimals=6 AND position=1 AND iou_supply=0`, reserve, usdt, iou2)
	eq(t, db, "iou supply", `SELECT count(*) FROM clear_iou_tokens WHERE address=$1 AND total_supply=5`, iou1)
	eq(t, db, "carol iou", `SELECT count(*) FROM clear_iou_balances WHERE token=$1 AND holder=$2 AND balance=5`, iou1, carol)
	// IOU tokens are discovered dynamically at AssetAdded and tracked in the registry.
	eq(t, db, "iou1 tracked (dynamic)", `SELECT count(*) FROM clear_contracts WHERE chain_id=1 AND address=$1 AND kind='iou' AND source='dynamic'`, iou1)
	eq(t, db, "iou2 tracked (dynamic)", `SELECT count(*) FROM clear_contracts WHERE chain_id=1 AND address=$1 AND kind='iou' AND source='dynamic'`, iou2)

	// swap history.
	eq(t, db, "reserve swap", `SELECT count(*) FROM clear_reserve_swaps WHERE reserve=$1 AND trader=$2 AND amount_in=100 AND amount_out=95 AND iou_total=5`, reserve, carol)
	if got := count(t, db, `SELECT count(*) FROM clear_reserve_swaps`); got != 1 {
		t.Errorf("reserve swaps = %d, want 1", got)
	}
	// activity: deposit x2, iou_minted x1, withdraw x1, single_deposit x1, rebalance x1
	// (Swap is NOT activity).
	if got := count(t, db, `SELECT count(*) FROM clear_reserve_activity`); got != 6 {
		t.Errorf("reserve activity rows = %d, want 6", got)
	}
	eq(t, db, "withdraw activity", `SELECT count(*) FROM clear_reserve_activity WHERE action='withdraw' AND caller=$1 AND lp=100`, alice)

	// --- oracle state ---
	// Config folds in from OracleConfigured, price from ClearOracleRateChanged,
	// redemption from ClearOracleRedemptionPriceChanged; last_refresh is the block
	// timestamp of the latest oracle event (the adapter's PriceUpdated at block 107).
	// `oracle` stays the ClearOracle address (the adapter's PriceUpdated must not
	// clobber it).
	eq(t, db, "oracle usdc", `SELECT count(*) FROM clear_oracle_prices
		WHERE asset=$1 AND oracle=$2 AND enabled=true AND asset_decimals=6 AND oracle_decimals=8
		AND price_ttl=3600 AND price=99990000 AND redemption_price=100000000
		AND last_refresh=$3 AND last_block=107`,
		usdc, oracleAddr, blockTs(107))

	// price history: one point per ClearOracleRateChanged (block 106); the adapter's
	// PriceUpdated does not add a second point.
	eq(t, db, "oracle price point", `SELECT count(*) FROM clear_oracle_price_history
		WHERE id='1:106:1' AND asset=$1 AND block_number=106 AND block_timestamp=$2 AND price=99990000`,
		usdc, blockTs(106))
	if got := count(t, db, `SELECT count(*) FROM clear_oracle_price_history WHERE asset=$1`, usdc); got != 1 {
		t.Errorf("oracle price points = %d, want 1", got)
	}

	// --- curve state ---
	// supply = 1000 - 200 = 800; 1 swap.
	eq(t, db, "curve pool", `SELECT count(*) FROM clear_curve_pools WHERE address=$1 AND lp_supply=800 AND swap_count=1`, pool)
	eq(t, db, "dave curve LP", `SELECT count(*) FROM clear_curve_lp_balances WHERE pool=$1 AND holder=$2 AND balance=800`, pool, dave)
	eq(t, db, "curve swap", `SELECT count(*) FROM clear_curve_swaps WHERE pool=$1 AND buyer=$2 AND sold_id=0 AND tokens_sold=100 AND bought_id=1 AND tokens_bought=99`, pool, eve)
	// liquidity: add + remove_one; the array arg round-trips through JSONB.
	if got := count(t, db, `SELECT count(*) FROM clear_curve_liquidity WHERE pool=$1`, pool); got != 2 {
		t.Errorf("curve liquidity rows = %d, want 2", got)
	}
	eq(t, db, "add liquidity json", `SELECT count(*) FROM clear_curve_liquidity WHERE pool=$1 AND kind='add' AND token_amounts='["500","500"]'::jsonb AND token_supply=1000`, pool)
	eq(t, db, "remove_one", `SELECT count(*) FROM clear_curve_liquidity WHERE pool=$1 AND kind='remove_one' AND token_amount=200 AND coin_amount=200`, pool)

	// --- reserve governance settings ---
	// One row per reserve; each event patched only its own column(s), and the ones
	// never emitted (e.g. flash_fee_bps) stay NULL rather than defaulting to 0.
	eq(t, db, "reserve settings", `SELECT count(*) FROM clear_reserve_settings
		 WHERE reserve=$1 AND kind='base' AND single_asset_fee_bps=7
		 AND swap_spread_min_bps=3 AND swap_spread_max_bps=500
		 AND iou_trader_bps=2000 AND iou_treasury_bps=4000
		 AND rebalance_trigger_bps=7000 AND flash_fee_bps IS NULL`, reserve)

	// --- IOU treasury fee (only ClearIOUMinted carries the split) ---
	eq(t, db, "iou treasury fee", `SELECT count(*) FROM clear_iou_tokens WHERE address=$1 AND total_supply=5 AND treasury_fees=1`, iou1)

	// --- factory: protocol config + reserve discovery ---
	eq(t, db, "protocol config", `SELECT count(*) FROM clear_protocol_config
		 WHERE factory=$1 AND treasury=$2 AND meta_reserve_implementation=$3 AND meta_reserve_version=2`,
		factory, treasuryAddr, implAddr)
	eq(t, db, "meta reserve row", `SELECT count(*) FROM clear_reserves
		 WHERE address=$1 AND kind='meta' AND name='Clear Meta USD' AND symbol='cmUSD'
		 AND reserve_index=1 AND implementation=$2 AND factory=$3
		 AND tokens = $4::jsonb`,
		metaReserve, implAddr, factory, `["`+reserve+`","`+nativeTok+`"]`)
	// The meta reserve was never configured: it is tracked purely from NewClearReserve.
	eq(t, db, "meta reserve tracked (dynamic)", `SELECT count(*) FROM clear_contracts
		 WHERE chain_id=1 AND address=$1 AND kind='meta_reserve' AND source='dynamic'`, metaReserve)

	// --- meta reserve legs: AssetAdded now carries each leg's target weight ---
	eq(t, db, "meta native leg", `SELECT count(*) FROM clear_reserve_assets
		 WHERE reserve=$1 AND asset=$2 AND iou=$3 AND decimals=6 AND position=0 AND weight=2000`,
		metaReserve, nativeTok, metaIou1)
	eq(t, db, "meta baselp leg", `SELECT count(*) FROM clear_reserve_assets
		 WHERE reserve=$1 AND asset=$2 AND iou=$3 AND decimals=18 AND position=1 AND weight=8000`,
		metaReserve, reserve, metaIou2)
	eq(t, db, "meta iou tracked", `SELECT count(*) FROM clear_contracts
		 WHERE chain_id=1 AND address=$1 AND kind='iou' AND source='dynamic'`, metaIou1)
	// weight is meta-only: a base reserve's assets have no target weight.
	if got := count(t, db, `SELECT count(*) FROM clear_reserve_assets WHERE reserve=$1 AND weight IS NOT NULL`, reserve); got != 0 {
		t.Errorf("base reserve assets with a weight = %d, want 0", got)
	}

	// --- curve pool deployer: pools linked back to their reserve ---
	eq(t, db, "base pool metadata", `SELECT count(*) FROM clear_curve_pools
		 WHERE address=$1 AND reserve=$2 AND base_pool=$1 AND is_base_pool AND coin IS NULL AND deployer=$3`,
		pool, reserve, deployer)
	eq(t, db, "iou metapool metadata", `SELECT count(*) FROM clear_curve_pools
		 WHERE address=$1 AND reserve=$2 AND base_pool=$3 AND is_base_pool = FALSE AND coin=$4`,
		iouPool, reserve, pool, iou1)
	eq(t, db, "iou metapool tracked (dynamic)", `SELECT count(*) FROM clear_contracts
		 WHERE chain_id=1 AND address=$1 AND kind='curve' AND source='dynamic'`, iouPool)
	// The base pool was seeded from config; PoolDeployed must not downgrade its row.
	eq(t, db, "base pool still config-sourced", `SELECT count(*) FROM clear_contracts
		 WHERE chain_id=1 AND address=$1 AND kind='curve' AND source='config'`, pool)

	// --- idempotency: redelivering logs must not double-apply ---
	redeliver := []exporter.LogEvent{
		mkLog(100, 2, R, reserve, "Transfer", map[string]string{"from": zeroAddr, "to": alice, "value": "1000"}),
		mkLog(100, 3, R, reserve, "Deposit", map[string]string{"caller": alice, "receiver": alice, "lpMinted": "1000", "amounts": `["1000","1000"]`}),
		// treasury_fees is cumulative like the balances, so it must not double either.
		mkLog(108, 4, IOU, iou1, "ClearIOUMinted", map[string]string{"to": carol, "amount": "5", "treasuryFee": "1"}),
	}
	for _, l := range redeliver {
		if err := e.NewLogEvent(l); err != nil {
			t.Fatalf("redeliver %s: %v", l.Id, err)
		}
	}
	eq(t, db, "alice LP after redeliver (unchanged)", `SELECT count(*) FROM clear_reserve_lp_balances WHERE reserve=$1 AND holder=$2 AND balance=$3`, reserve, alice, "700")
	eq(t, db, "reserve supply after redeliver (unchanged)", `SELECT count(*) FROM clear_reserves WHERE address=$1 AND lp_supply=1450 AND total_deposits=1550`, reserve)
	eq(t, db, "usdc holdings after redeliver (unchanged)", `SELECT count(*) FROM clear_reserve_token_balances WHERE reserve=$1 AND asset=$2 AND balance=$3`, reserve, usdc, "1525")
	eq(t, db, "iou treasury fee after redeliver (unchanged)", `SELECT count(*) FROM clear_iou_tokens WHERE address=$1 AND treasury_fees=1`, iou1)
}
