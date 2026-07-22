package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	exporter "github.com/evmi-cloud/go-evm-indexer/pkg/exporter"
)

// --- registry upserts ---

func ensureReserve(tx *sql.Tx, addr, kind string, block uint64) error {
	_, err := tx.Exec(`INSERT INTO clear_reserves (address, kind, first_block, last_block)
VALUES ($1, $2, $3, $3)
ON CONFLICT (address) DO UPDATE SET last_block = GREATEST(clear_reserves.last_block, EXCLUDED.last_block)`,
		addr, kind, block)
	return err
}

func ensureCurve(tx *sql.Tx, addr string, block uint64) error {
	_, err := tx.Exec(`INSERT INTO clear_curve_pools (address, first_block, last_block)
VALUES ($1, $2, $2)
ON CONFLICT (address) DO UPDATE SET last_block = GREATEST(clear_curve_pools.last_block, EXCLUDED.last_block)`,
		addr, block)
	return err
}

func ensureIou(tx *sql.Tx, addr string, block uint64) error {
	_, err := tx.Exec(`INSERT INTO clear_iou_tokens (address, last_block)
VALUES ($1, $2)
ON CONFLICT (address) DO UPDATE SET last_block = GREATEST(clear_iou_tokens.last_block, EXCLUDED.last_block)`,
		addr, block)
	return err
}

func exec(tx *sql.Tx, q string, args ...any) error {
	_, err := tx.Exec(q, args...)
	return err
}

// nullNum wraps a numeric arg for a nullable NUMERIC/BIGINT column.
func nullNum(args map[string]string, key string) sql.NullString {
	if v, ok := args[key]; ok && v != "" {
		return sql.NullString{String: v, Valid: true}
	}
	return sql.NullString{}
}

// --- ERC20 Transfer (reserve LP / IOU / curve LP), routed by contract kind ---

func handleTransfer(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	from := normAddr(firstArg(log.Args, "from", "sender"))
	to := normAddr(firstArg(log.Args, "to", "receiver"))
	val := firstArg(log.Args, "value")
	if val == "" {
		return nil
	}
	owner := normAddr(log.Address)
	block := log.BlockNumber

	var balTable, keyCol, parentTable string
	switch k {
	case baseReserveKind, metaReserveKind:
		if err := ensureReserve(tx, owner, k.reserveType(), block); err != nil {
			return err
		}
		balTable, keyCol, parentTable = "clear_reserve_lp_balances", "reserve", "clear_reserves"
	case iouKind:
		if err := ensureIou(tx, owner, block); err != nil {
			return err
		}
		balTable, keyCol, parentTable = "clear_iou_balances", "token", "clear_iou_tokens"
	case curveKind:
		if err := ensureCurve(tx, owner, block); err != nil {
			return err
		}
		balTable, keyCol, parentTable = "clear_curve_lp_balances", "pool", "clear_curve_pools"
	default:
		return nil // Transfer on an unclassified contract — ignore.
	}

	supplyCol := "lp_supply"
	if k == iouKind {
		supplyCol = "total_supply"
	}
	addSupply := func(delta string) error {
		if err := exec(tx, "UPDATE "+parentTable+" SET "+supplyCol+" = "+supplyCol+" + $2, last_block = $3 WHERE address = $1",
			owner, delta, block); err != nil {
			return err
		}
		// Mirror IOU supply onto its reserve-asset registry row (1 IOU per asset).
		if k == iouKind {
			return exec(tx, "UPDATE clear_reserve_assets SET iou_supply = iou_supply + $2 WHERE iou = $1", owner, delta)
		}
		return nil
	}

	switch {
	case isZeroAddr(from): // mint
		if err := adjustBalance(tx, balTable, keyCol, owner, to, val, block); err != nil {
			return err
		}
		return addSupply(val)
	case isZeroAddr(to): // burn
		if err := adjustBalance(tx, balTable, keyCol, owner, from, neg(val), block); err != nil {
			return err
		}
		return addSupply(neg(val))
	default: // transfer
		if err := adjustBalance(tx, balTable, keyCol, owner, from, neg(val), block); err != nil {
			return err
		}
		return adjustBalance(tx, balTable, keyCol, owner, to, val, block)
	}
}

// --- reserve physical token balances (base reserves only) ---
//
// clear_reserve_token_balances reconstructs the reserve's real ERC20 holdings from
// the token flows carried by its events. Only base reserves are tracked: their
// assets come from AssetAdded (positioned in assetList order), whereas a meta
// reserve holds base-LP + native, which are not in that registry.

// reserveAssetsByPosition returns the reserve's assets keyed by their assetList
// index, so a Deposit/Withdraw `amounts[i]` can be routed to the right token.
func reserveAssetsByPosition(tx *sql.Tx, reserve string) (map[int]string, error) {
	rows, err := tx.Query(`SELECT position, asset FROM clear_reserve_assets WHERE reserve = $1 AND position IS NOT NULL`, reserve)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var pos int
		var asset string
		if err := rows.Scan(&pos, &asset); err != nil {
			return nil, err
		}
		out[pos] = asset
	}
	return out, rows.Err()
}

// applyReserveAmounts adjusts each underlying-token balance from a Deposit/Withdraw
// `amounts` array (evmi serializes uint256[] as a JSON array of decimal strings),
// aligned with assetList order. `negate` flips the sign for withdrawals. An index
// whose asset is unknown (AssetAdded missed because indexing started late) is
// skipped — its balance would be wrong anyway.
func applyReserveAmounts(tx *sql.Tx, reserve, amountsJSON string, negate bool, block uint64) error {
	if amountsJSON == "" {
		return nil
	}
	var amounts []string
	if err := json.Unmarshal([]byte(amountsJSON), &amounts); err != nil {
		return fmt.Errorf("amounts array %q: %w", amountsJSON, err)
	}
	byPos, err := reserveAssetsByPosition(tx, reserve)
	if err != nil {
		return err
	}
	for i, amt := range amounts {
		asset := byPos[i]
		if asset == "" {
			continue
		}
		delta := amt
		if negate {
			delta = neg(amt)
		}
		if err := adjustTokenBalance(tx, reserve, asset, delta, block); err != nil {
			return err
		}
	}
	return nil
}

// --- reserve events ---

func insertActivity(tx *sql.Tx, log exporter.LogEvent, reserve, action, caller, receiver, asset, amount, amount2, fee, lp string) error {
	return exec(tx, `INSERT INTO clear_reserve_activity
(id, reserve, block_number, log_index, tx_hash, action, caller, receiver, asset, amount, amount2, fee, lp)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (id) DO NOTHING`,
		log.Id, reserve, log.BlockNumber, log.LogIndex, log.TransactionHash, action,
		caller, receiver, nullEmpty(asset), amount, amount2, fee, lp)
}

func nullEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func bumpReserve(tx *sql.Tx, reserve, kind, col, delta string, block uint64) error {
	if err := ensureReserve(tx, reserve, kind, block); err != nil {
		return err
	}
	return exec(tx, "UPDATE clear_reserves SET "+col+" = "+col+" + $2, last_block = $3 WHERE address = $1",
		reserve, delta, block)
}

func handleReserveDeposit(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	lp := firstArg(a, "metaLpMinted", "lpMinted")
	if err := insertActivity(tx, log, reserve, "deposit",
		normAddr(a["caller"]), normAddr(a["receiver"]), "",
		firstArg(a, "baseLpIn", "lpMinted"), num(a, "nativeIn"), "0", numOrZero(lp)); err != nil {
		return err
	}
	if k == baseReserveKind {
		if err := applyReserveAmounts(tx, reserve, a["amounts"], false, log.BlockNumber); err != nil {
			return err
		}
	}
	return bumpReserve(tx, reserve, k.reserveType(), "total_deposits", numOrZero(lp), log.BlockNumber)
}

func handleReserveWithdraw(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	lp := firstArg(a, "metaLpBurned", "lpBurned")
	if err := insertActivity(tx, log, reserve, "withdraw",
		normAddr(a["caller"]), normAddr(a["receiver"]), "",
		firstArg(a, "baseLpOut", "lpBurned"), num(a, "nativeOut"), "0", numOrZero(lp)); err != nil {
		return err
	}
	if k == baseReserveKind {
		if err := applyReserveAmounts(tx, reserve, a["amounts"], true, log.BlockNumber); err != nil {
			return err
		}
	}
	return bumpReserve(tx, reserve, k.reserveType(), "total_withdrawals", numOrZero(lp), log.BlockNumber)
}

func handleSingleAssetDeposit(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	lp := firstArg(a, "metaLpMinted", "lpMinted")
	if err := insertActivity(tx, log, reserve, "single_deposit",
		normAddr(a["caller"]), normAddr(a["receiver"]), normAddr(a["asset"]),
		num(a, "amountIn"), "0", num(a, "fee"), numOrZero(lp)); err != nil {
		return err
	}
	if k == baseReserveKind {
		// amountIn pulled in; fee forwarded to treasury — net (amountIn - fee) stays.
		asset := normAddr(a["asset"])
		if err := adjustTokenBalance(tx, reserve, asset, num(a, "amountIn"), log.BlockNumber); err != nil {
			return err
		}
		if err := adjustTokenBalance(tx, reserve, asset, neg(num(a, "fee")), log.BlockNumber); err != nil {
			return err
		}
	}
	return bumpReserve(tx, reserve, k.reserveType(), "total_deposits", numOrZero(lp), log.BlockNumber)
}

func handleSingleAssetWithdraw(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	lp := firstArg(a, "metaLpBurned", "lpBurned")
	if err := insertActivity(tx, log, reserve, "single_withdraw",
		normAddr(a["caller"]), normAddr(a["receiver"]), normAddr(a["asset"]),
		num(a, "amountOut"), "0", num(a, "fee"), numOrZero(lp)); err != nil {
		return err
	}
	if k == baseReserveKind {
		// amountOut sent to receiver and fee to treasury both leave the reserve.
		asset := normAddr(a["asset"])
		if err := adjustTokenBalance(tx, reserve, asset, neg(num(a, "amountOut")), log.BlockNumber); err != nil {
			return err
		}
		if err := adjustTokenBalance(tx, reserve, asset, neg(num(a, "fee")), log.BlockNumber); err != nil {
			return err
		}
	}
	return bumpReserve(tx, reserve, k.reserveType(), "total_withdrawals", numOrZero(lp), log.BlockNumber)
}

func handleRebalanced(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	if err := ensureReserve(tx, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	if k == baseReserveKind {
		if err := adjustTokenBalance(tx, reserve, normAddr(a["tokenIn"]), num(a, "amountIn"), log.BlockNumber); err != nil {
			return err
		}
		if err := adjustTokenBalance(tx, reserve, normAddr(a["tokenOut"]), neg(num(a, "amountOut")), log.BlockNumber); err != nil {
			return err
		}
	}
	return insertActivity(tx, log, reserve, "rebalance",
		normAddr(a["caller"]), normAddr(a["recipient"]), normAddr(a["tokenIn"]),
		num(a, "amountIn"), num(a, "amountOut"), "0", "0")
}

func handleFlashLoan(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	if err := ensureReserve(tx, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	if k == baseReserveKind {
		// Principal is lent and returned within the tx; only the fee stays in the reserve.
		if err := adjustTokenBalance(tx, reserve, normAddr(a["token"]), num(a, "fee"), log.BlockNumber); err != nil {
			return err
		}
	}
	return insertActivity(tx, log, reserve, "flash_loan",
		normAddr(a["initiator"]), normAddr(a["receiver"]), normAddr(a["token"]),
		num(a, "amount"), "0", num(a, "fee"), "0")
}

func handleIOUMinted(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	amount := num(a, "amount")
	if err := insertActivity(tx, log, reserve, "iou_minted",
		normAddr(a["caller"]), normAddr(a["receiver"]), normAddr(a["asset"]),
		amount, "0", "0", "0"); err != nil {
		return err
	}
	if k == baseReserveKind {
		// mintIOU pulls `amount` of the underlying into the reserve (1:1 backing).
		if err := adjustTokenBalance(tx, reserve, normAddr(a["asset"]), amount, log.BlockNumber); err != nil {
			return err
		}
	}
	return bumpReserve(tx, reserve, k.reserveType(), "iou_minted", amount, log.BlockNumber)
}

func handleIOURedeemed(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	amount := num(a, "amount")
	if err := insertActivity(tx, log, reserve, "iou_redeemed",
		normAddr(a["holder"]), normAddr(a["receiver"]), normAddr(a["asset"]),
		amount, "0", "0", "0"); err != nil {
		return err
	}
	if k == baseReserveKind {
		// redeemIOU pays out `amount` of the underlying (par settlement, 1:1).
		if err := adjustTokenBalance(tx, reserve, normAddr(a["asset"]), neg(amount), log.BlockNumber); err != nil {
			return err
		}
	}
	return bumpReserve(tx, reserve, k.reserveType(), "iou_redeemed", amount, log.BlockNumber)
}

func handleReserveSwap(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	if err := ensureReserve(tx, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	if err := exec(tx, `INSERT INTO clear_reserve_swaps
(id, reserve, block_number, log_index, tx_hash, trader, token_in, token_out, recipient,
 amount_in, amount_out, iou_total, trader_iou, treasury_iou, lp_iou)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (id) DO NOTHING`,
		log.Id, reserve, log.BlockNumber, log.LogIndex, log.TransactionHash,
		normAddr(a["trader"]), normAddr(a["tokenIn"]), normAddr(a["tokenOut"]), normAddr(a["recipient"]),
		num(a, "amountIn"), num(a, "amountOut"), num(a, "iouTotal"), num(a, "traderIOU"), num(a, "treasuryIOU"), num(a, "lpIOU")); err != nil {
		return err
	}
	if k == baseReserveKind {
		// amountIn of tokenIn received, amountOut of tokenOut paid out. The IOU
		// shortfall is a claim minted against NAV, not an underlying-token movement.
		if err := adjustTokenBalance(tx, reserve, normAddr(a["tokenIn"]), num(a, "amountIn"), log.BlockNumber); err != nil {
			return err
		}
		if err := adjustTokenBalance(tx, reserve, normAddr(a["tokenOut"]), neg(num(a, "amountOut")), log.BlockNumber); err != nil {
			return err
		}
	}
	return exec(tx, `UPDATE clear_reserves SET swap_count = swap_count + 1, last_block = $2 WHERE address = $1`,
		reserve, log.BlockNumber)
}

func handleAssetAdded(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	iou := normAddr(a["iou"])
	if err := ensureReserve(tx, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	if iou != "" && !isZeroAddr(iou) {
		if err := ensureIou(tx, iou, log.BlockNumber); err != nil {
			return err
		}
	}
	// position = current asset count for the reserve, so it mirrors the contract's
	// append-only assetList index; kept unchanged on a re-emit (assets are added once).
	return exec(tx, `INSERT INTO clear_reserve_assets (reserve, asset, decimals, iou, position)
VALUES ($1, $2, $3, $4, (SELECT count(*) FROM clear_reserve_assets WHERE reserve = $1))
ON CONFLICT (reserve, asset) DO UPDATE SET decimals = EXCLUDED.decimals, iou = EXCLUDED.iou`,
		reserve, normAddr(a["asset"]), nullNum(a, "decimals"), nullEmpty(iou))
}

// --- curve pool events ---

func handleCurveSwap(tx *sql.Tx, log exporter.LogEvent, underlying bool) error {
	a := log.Args
	pool := normAddr(log.Address)
	if err := ensureCurve(tx, pool, log.BlockNumber); err != nil {
		return err
	}
	if err := exec(tx, `INSERT INTO clear_curve_swaps
(id, pool, block_number, log_index, tx_hash, buyer, sold_id, tokens_sold, bought_id, tokens_bought, underlying)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (id) DO NOTHING`,
		log.Id, pool, log.BlockNumber, log.LogIndex, log.TransactionHash,
		normAddr(a["buyer"]), num(a, "sold_id"), num(a, "tokens_sold"),
		num(a, "bought_id"), num(a, "tokens_bought"), underlying); err != nil {
		return err
	}
	return exec(tx, `UPDATE clear_curve_pools SET swap_count = swap_count + 1, last_block = $2 WHERE address = $1`,
		pool, log.BlockNumber)
}

func handleCurveLiquidity(tx *sql.Tx, log exporter.LogEvent, kind string) error {
	a := log.Args
	pool := normAddr(log.Address)
	if err := ensureCurve(tx, pool, log.BlockNumber); err != nil {
		return err
	}
	return exec(tx, `INSERT INTO clear_curve_liquidity
(id, pool, block_number, log_index, tx_hash, provider, kind,
 token_amounts, fees, token_id, token_amount, coin_amount, invariant, token_supply)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
ON CONFLICT (id) DO NOTHING`,
		log.Id, pool, log.BlockNumber, log.LogIndex, log.TransactionHash,
		normAddr(a["provider"]), kind,
		jsonArg(a, "token_amounts"), jsonArg(a, "fees"),
		nullNum(a, "token_id"), nullNum(a, "token_amount"), nullNum(a, "coin_amount"),
		nullNum(a, "invariant"), nullNum(a, "token_supply"))
}

// numOrZero normalizes an amount that may be absent to "0".
func numOrZero(v string) string {
	if v == "" {
		return "0"
	}
	return v
}

// --- oracle state (one row per asset, keyed by asset) ---
//
// The ClearOracle emits config (OracleConfigured), price (ClearOracleRateChanged)
// and redemption price (ClearOracleRedemptionPriceChanged); the PythOracleAdapter
// emits PriceUpdated. All four fold into clear_oracle_prices by asset, and each
// stamps last_refresh with the event's block timestamp (the last refresh date).
// Each handler upserts only its own columns so it can create the row or patch an
// existing one in any order.

// parseBool decodes an indexed bool arg, tolerating "true"/"false", "1"/"0" and the
// 32-byte hex word form evmi may hand back. Absent -> NULL.
func parseBool(s string) sql.NullBool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return sql.NullBool{}
	}
	switch s {
	case "true", "1", "0x1":
		return sql.NullBool{Bool: true, Valid: true}
	case "false", "0", "0x0":
		return sql.NullBool{Bool: false, Valid: true}
	}
	if strings.HasPrefix(s, "0x") { // hex word: any non-zero digit => true
		return sql.NullBool{Bool: strings.Trim(s[2:], "0") != "", Valid: true}
	}
	return sql.NullBool{}
}

func handleOracleConfigured(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	asset := normAddr(a["asset"])
	if asset == "" {
		return nil
	}
	return exec(tx, `INSERT INTO clear_oracle_prices
(asset, oracle, enabled, asset_decimals, oracle_decimals, price_ttl, redemption_price, last_refresh, last_block)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (asset) DO UPDATE SET
  oracle = EXCLUDED.oracle,
  enabled = EXCLUDED.enabled,
  asset_decimals = EXCLUDED.asset_decimals,
  oracle_decimals = EXCLUDED.oracle_decimals,
  price_ttl = EXCLUDED.price_ttl,
  redemption_price = EXCLUDED.redemption_price,
  last_refresh = EXCLUDED.last_refresh,
  last_block = EXCLUDED.last_block`,
		asset, normAddr(log.Address), parseBool(a["enabled"]),
		nullNum(a, "assetDecimals"), nullNum(a, "oracleDecimals"),
		nullNum(a, "priceTTL"), nullNum(a, "redemptionPrice"), log.BlockTimestamp, log.BlockNumber)
}

func handleOracleRate(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	asset := normAddr(a["asset"])
	if asset == "" {
		return nil
	}
	return exec(tx, `INSERT INTO clear_oracle_prices (asset, oracle, price, last_refresh, last_block)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (asset) DO UPDATE SET
  oracle = EXCLUDED.oracle, price = EXCLUDED.price,
  last_refresh = EXCLUDED.last_refresh, last_block = EXCLUDED.last_block`,
		asset, normAddr(log.Address), num(a, "price"), log.BlockTimestamp, log.BlockNumber)
}

func handleOracleRedemption(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	asset := normAddr(a["asset"])
	if asset == "" {
		return nil
	}
	return exec(tx, `INSERT INTO clear_oracle_prices (asset, redemption_price, last_refresh, last_block)
VALUES ($1,$2,$3,$4)
ON CONFLICT (asset) DO UPDATE SET
  redemption_price = EXCLUDED.redemption_price,
  last_refresh = EXCLUDED.last_refresh, last_block = EXCLUDED.last_block`,
		asset, num(a, "redemptionPrice"), log.BlockTimestamp, log.BlockNumber)
}

// handleOraclePublish records the adapter's price refresh. It does not overwrite
// `oracle` — log.Address here is the adapter, not the ClearOracle.
func handleOraclePublish(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	asset := normAddr(a["asset"])
	if asset == "" {
		return nil
	}
	return exec(tx, `INSERT INTO clear_oracle_prices (asset, price, last_refresh, last_block)
VALUES ($1,$2,$3,$4)
ON CONFLICT (asset) DO UPDATE SET
  price = EXCLUDED.price, last_refresh = EXCLUDED.last_refresh, last_block = EXCLUDED.last_block`,
		asset, num(a, "price"), log.BlockTimestamp, log.BlockNumber)
}
