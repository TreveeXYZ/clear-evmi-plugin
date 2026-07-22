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
)

var allTables = []string{
	"clear_processed_events", "clear_reserves", "clear_reserve_lp_balances",
	"clear_reserve_assets", "clear_reserve_token_balances", "clear_reserve_swaps",
	"clear_reserve_activity", "clear_iou_tokens", "clear_iou_balances",
	"clear_curve_pools", "clear_curve_lp_balances", "clear_curve_swaps",
	"clear_curve_liquidity",
}

func mkLog(block, idx uint64, contract, addr, event string, args map[string]string) exporter.LogEvent {
	return exporter.LogEvent{
		Id:              fmt.Sprintf("1:%d:%d", block, idx),
		ChainId:         1,
		ContractName:    contract,
		EventName:       event,
		Address:         addr,
		Args:            args,
		BlockNumber:     block,
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

	e := &clearExporter{}
	cfg, _ := json.Marshal(pluginConfig{Dsn: dsn})
	if err := e.Init(exporter.Context{ExporterName: "clear-defi-test", PipelineId: 1, ChainId: 1, Config: cfg}); err != nil {
		t.Fatalf("init: %v", err)
	}
	defer e.Close()
	db := e.db

	const R = "ClearBaseReserve"
	const IOU = "ClearIOU"
	const CURVE = "CurveStableSwapNG"

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
		mkLog(101, 1, R, reserve, "Deposit", map[string]string{"caller": bob, "receiver": bob, "lpMinted": "500", "amounts": `["500","500"]`}),
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
			"provider": dave, "token_amounts": `["500","500"]`, "fees": `["0","0"]`, "invariant": "1000", "token_supply": "1000"}),
		mkLog(104, 2, CURVE, pool, "TokenExchange", map[string]string{
			"buyer": eve, "sold_id": "0", "tokens_sold": "100", "bought_id": "1", "tokens_bought": "99"}),
		mkLog(105, 0, CURVE, pool, "Transfer", map[string]string{"sender": dave, "receiver": zeroAddr, "value": "200"}),
		mkLog(105, 1, CURVE, pool, "RemoveLiquidityOne", map[string]string{
			"provider": dave, "token_id": "0", "token_amount": "200", "coin_amount": "200", "token_supply": "800"}),
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

	// asset registry + IOU; position mirrors assetList order and drives amounts[] mapping.
	eq(t, db, "asset usdc", `SELECT count(*) FROM clear_reserve_assets WHERE reserve=$1 AND asset=$2 AND iou=$3 AND decimals=6 AND position=0`, reserve, usdc, iou1)
	eq(t, db, "asset usdt", `SELECT count(*) FROM clear_reserve_assets WHERE reserve=$1 AND asset=$2 AND iou=$3 AND decimals=6 AND position=1`, reserve, usdt, iou2)
	eq(t, db, "iou supply", `SELECT count(*) FROM clear_iou_tokens WHERE address=$1 AND total_supply=5`, iou1)
	eq(t, db, "carol iou", `SELECT count(*) FROM clear_iou_balances WHERE token=$1 AND holder=$2 AND balance=5`, iou1, carol)

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

	// --- idempotency: redelivering logs must not double-apply ---
	redeliver := []exporter.LogEvent{
		mkLog(100, 2, R, reserve, "Transfer", map[string]string{"from": zeroAddr, "to": alice, "value": "1000"}),
		mkLog(100, 3, R, reserve, "Deposit", map[string]string{"caller": alice, "receiver": alice, "lpMinted": "1000", "amounts": `["1000","1000"]`}),
	}
	for _, l := range redeliver {
		if err := e.NewLogEvent(l); err != nil {
			t.Fatalf("redeliver %s: %v", l.Id, err)
		}
	}
	eq(t, db, "alice LP after redeliver (unchanged)", `SELECT count(*) FROM clear_reserve_lp_balances WHERE reserve=$1 AND holder=$2 AND balance=$3`, reserve, alice, "700")
	eq(t, db, "reserve supply after redeliver (unchanged)", `SELECT count(*) FROM clear_reserves WHERE address=$1 AND lp_supply=1450 AND total_deposits=1550`, reserve)
	eq(t, db, "usdc holdings after redeliver (unchanged)", `SELECT count(*) FROM clear_reserve_token_balances WHERE reserve=$1 AND asset=$2 AND balance=$3`, reserve, usdc, "1525")
}
