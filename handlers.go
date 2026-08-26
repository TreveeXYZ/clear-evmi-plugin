package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	exporter "github.com/evmi-cloud/go-evm-indexer/pkg/exporter"
)

// --- registry upserts ---

func ensureReserve(tx *sql.Tx, chainID uint64, addr, kind string, block uint64) error {
	_, err := tx.Exec(`INSERT INTO clear_reserves (chain_id, address, kind, first_block, last_block)
VALUES ($1, $2, $3, $4, $4)
ON CONFLICT (chain_id, address) DO UPDATE SET last_block = GREATEST(clear_reserves.last_block, EXCLUDED.last_block)`,
		chainID, addr, kind, block)
	return err
}

func ensureCurve(tx *sql.Tx, chainID uint64, addr string, block uint64) error {
	_, err := tx.Exec(`INSERT INTO clear_curve_pools (chain_id, address, first_block, last_block)
VALUES ($1, $2, $3, $3)
ON CONFLICT (chain_id, address) DO UPDATE SET last_block = GREATEST(clear_curve_pools.last_block, EXCLUDED.last_block)`,
		chainID, addr, block)
	return err
}

func ensureIou(tx *sql.Tx, chainID uint64, addr string, block uint64) error {
	_, err := tx.Exec(`INSERT INTO clear_iou_tokens (chain_id, address, last_block)
VALUES ($1, $2, $3)
ON CONFLICT (chain_id, address) DO UPDATE SET last_block = GREATEST(clear_iou_tokens.last_block, EXCLUDED.last_block)`,
		chainID, addr, block)
	return err
}

// ensureToken registers an ERC20 in clear_tokens, reading its name/symbol/decimals
// off the chain the first time it is seen. It is called wherever a token address
// ENTERS the schema — a reserve's assets and their IOUs (AssetAdded), a reserve's
// own LP token and its token set (NewClearReserve), a Curve pool's LP token and
// the coins it pairs (PoolDeployed / Plain- & MetaPoolDeployed).
//
// The row is claimed first, and only the writer that actually inserted it goes on
// to spend an RPC round trip: a token already listed costs one indexed no-op
// INSERT and nothing else, which is what makes this safe to call from a hot path.
// The claim lives in the caller's transaction, so if the event later fails it
// rolls back with everything else and the token is fetched again on redelivery —
// no half-written row survives.
//
// hints are the fields the discovery event already carries; a complete set skips
// the fetch, and anything the getters do not answer falls back to them.
func (e *clearExporter) ensureToken(tx *sql.Tx, chainID uint64, addr string, block uint64, hints tokenMeta) error {
	addr = normAddr(addr)
	if addr == "" || isZeroAddr(addr) {
		return nil
	}

	var first sql.NullInt64
	if block > 0 {
		first = sql.NullInt64{Int64: int64(block), Valid: true}
	}
	res, err := tx.Exec(`INSERT INTO clear_tokens (chain_id, address, first_block)
VALUES ($1, $2, $3) ON CONFLICT (chain_id, address) DO NOTHING`, chainID, addr, first)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	created := n > 0

	meta := hints
	if created && e.rpc != nil && !meta.complete() {
		// A request-level failure is returned rather than swallowed: it fails the
		// event, the claim rolls back with it, and EVMI redelivers — otherwise a
		// blank row would stand for good and never be retried.
		fetched, err := fetchTokenMeta(e.rpc, addr)
		if err != nil {
			return fmt.Errorf("token %s: %w", addr, err)
		}
		meta = fetched.withFallback(hints)
	}
	if !created && meta.empty() {
		return nil // already known, and this call has nothing to add
	}

	// Patch only what is still missing, so a later event can fill a gap the
	// getters left — a token whose name()/symbol()/decimals() revert still gets
	// its decimals from the AssetAdded that names it, whichever came first.
	return exec(tx, `UPDATE clear_tokens SET
  name     = COALESCE(name, NULLIF($3::text, '')),
  symbol   = COALESCE(symbol, NULLIF($4::text, '')),
  decimals = COALESCE(decimals, $5)
WHERE chain_id = $1 AND address = $2`,
		chainID, addr, meta.name, meta.symbol, meta.decimals)
}

// ensureTokens registers a list of tokens, skipping empty/zero entries.
func (e *clearExporter) ensureTokens(tx *sql.Tx, chainID uint64, addrs []string, block uint64) error {
	for _, addr := range addrs {
		if err := e.ensureToken(tx, chainID, addr, block, tokenMeta{}); err != nil {
			return err
		}
	}
	return nil
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

// nullInt is nullNum for a small integer arg (an ERC20 `decimals`), NULL when the
// arg is absent or not a plain number.
func nullInt(args map[string]string, key string) sql.NullInt64 {
	v, err := strconv.ParseInt(strings.TrimSpace(args[key]), 10, 64)
	if err != nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
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
	chainID := log.ChainId

	var balTable, keyCol, parentTable string
	switch k {
	case baseReserveKind, metaReserveKind:
		if err := ensureReserve(tx, chainID, owner, k.reserveType(), block); err != nil {
			return err
		}
		balTable, keyCol, parentTable = "clear_reserve_lp_balances", "reserve", "clear_reserves"
	case iouKind:
		if err := ensureIou(tx, chainID, owner, block); err != nil {
			return err
		}
		balTable, keyCol, parentTable = "clear_iou_balances", "token", "clear_iou_tokens"
	case curveKind:
		if err := ensureCurve(tx, chainID, owner, block); err != nil {
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
		if err := exec(tx, "UPDATE "+parentTable+" SET "+supplyCol+" = "+supplyCol+" + $3, last_block = $4 WHERE chain_id = $1 AND address = $2",
			chainID, owner, delta, block); err != nil {
			return err
		}
		// Mirror IOU supply onto its reserve-asset registry row (1 IOU per asset).
		if k == iouKind {
			return exec(tx, "UPDATE clear_reserve_assets SET iou_supply = iou_supply + $3 WHERE chain_id = $1 AND iou = $2", chainID, owner, delta)
		}
		return nil
	}

	switch {
	case isZeroAddr(from): // mint
		if err := adjustBalance(tx, chainID, balTable, keyCol, owner, to, val, block); err != nil {
			return err
		}
		return addSupply(val)
	case isZeroAddr(to): // burn
		if err := adjustBalance(tx, chainID, balTable, keyCol, owner, from, neg(val), block); err != nil {
			return err
		}
		return addSupply(neg(val))
	default: // transfer
		if err := adjustBalance(tx, chainID, balTable, keyCol, owner, from, neg(val), block); err != nil {
			return err
		}
		return adjustBalance(tx, chainID, balTable, keyCol, owner, to, val, block)
	}
}

// --- reserve physical token balances (base reserves only) ---
//
// clear_reserve_token_balances reconstructs the reserve's real ERC20 holdings from
// the token flows carried by its events. Only base reserves are tracked: their
// assets come from AssetAdded positioned in assetList order, which is what makes a
// Deposit/Withdraw `amounts[i]` resolvable to a token. A meta reserve also emits
// AssetAdded (one per leg, since the "add some logs" contract change), so its legs
// ARE registered — but its balanced Deposit/Withdraw carry named scalars
// (baseLpIn/nativeIn, baseLpOut/nativeOut) instead of a positional amounts array,
// with nothing in the event tying a scalar to a leg address. Its physical holdings
// are therefore still not reconstructed.

// reserveAssetsByPosition returns the reserve's assets keyed by their assetList
// index, so a Deposit/Withdraw `amounts[i]` can be routed to the right token.
func reserveAssetsByPosition(tx *sql.Tx, chainID uint64, reserve string) (map[int]string, error) {
	rows, err := tx.Query(`SELECT position, asset FROM clear_reserve_assets WHERE chain_id = $1 AND reserve = $2 AND position IS NOT NULL`, chainID, reserve)
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
// `amounts` array, aligned with assetList order. `negate` flips the sign for
// withdrawals. An index whose asset is unknown (AssetAdded missed because indexing
// started late) is skipped — its balance would be wrong anyway.
func applyReserveAmounts(tx *sql.Tx, chainID uint64, reserve, amountsArg string, negate bool, block uint64) error {
	amounts, err := splitArrayArg(amountsArg)
	if err != nil {
		return fmt.Errorf("amounts array: %w", err)
	}
	if len(amounts) == 0 {
		return nil
	}
	byPos, err := reserveAssetsByPosition(tx, chainID, reserve)
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
		if err := adjustTokenBalance(tx, chainID, reserve, asset, delta, block); err != nil {
			return err
		}
	}
	return nil
}

// snapshotReserveValue writes the reserve's daily value point for charting:
// total_assets = Σ balance*10^(18-decimals) (par-valued gross holdings, 18-dec —
// the contract's totalAssets()) and total_supply (LP supply), bucketed by the UTC
// day of the block timestamp. One row per (reserve, day); the guard keeps the
// highest-block snapshot so each day holds its end-of-day value. Base reserves
// only. Call it after this event's balances/supply are applied.
func snapshotReserveValue(tx *sql.Tx, reserve string, log exporter.LogEvent) error {
	_, err := tx.Exec(`INSERT INTO clear_reserve_value_history
(chain_id, reserve, day, block_number, block_timestamp, total_assets, total_supply)
SELECT $4, $1, (to_timestamp($2) AT TIME ZONE 'UTC')::date, $3, $2,
  COALESCE((SELECT sum(b.balance * power(10::numeric, 18 - COALESCE(a.decimals, 18)))
            FROM clear_reserve_token_balances b
            JOIN clear_reserve_assets a ON a.chain_id = b.chain_id AND a.reserve = b.reserve AND a.asset = b.asset
            WHERE b.chain_id = $4 AND b.reserve = $1), 0),
  COALESCE((SELECT lp_supply FROM clear_reserves WHERE chain_id = $4 AND address = $1), 0)
ON CONFLICT (chain_id, reserve, day) DO UPDATE SET
  block_number = EXCLUDED.block_number,
  block_timestamp = EXCLUDED.block_timestamp,
  total_assets = EXCLUDED.total_assets,
  total_supply = EXCLUDED.total_supply
WHERE clear_reserve_value_history.block_number <= EXCLUDED.block_number`,
		reserve, log.BlockTimestamp, log.BlockNumber, log.ChainId)
	return err
}

// --- reserve events ---

func insertActivity(tx *sql.Tx, log exporter.LogEvent, reserve, action, caller, receiver, asset, amount, amount2, fee, lp string) error {
	return exec(tx, `INSERT INTO clear_reserve_activity
(id, chain_id, reserve, block_number, log_index, tx_hash, action, caller, receiver, asset, amount, amount2, fee, lp)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
ON CONFLICT (id) DO NOTHING`,
		log.Id, log.ChainId, reserve, log.BlockNumber, log.LogIndex, log.TransactionHash, action,
		caller, receiver, nullEmpty(asset), amount, amount2, fee, lp)
}

func nullEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func bumpReserve(tx *sql.Tx, chainID uint64, reserve, kind, col, delta string, block uint64) error {
	if err := ensureReserve(tx, chainID, reserve, kind, block); err != nil {
		return err
	}
	return exec(tx, "UPDATE clear_reserves SET "+col+" = "+col+" + $3, last_block = $4 WHERE chain_id = $1 AND address = $2",
		chainID, reserve, delta, block)
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
		if err := applyReserveAmounts(tx, log.ChainId, reserve, a["amounts"], false, log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
			return err
		}
	}
	return bumpReserve(tx, log.ChainId, reserve, k.reserveType(), "total_deposits", numOrZero(lp), log.BlockNumber)
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
		if err := applyReserveAmounts(tx, log.ChainId, reserve, a["amounts"], true, log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
			return err
		}
	}
	return bumpReserve(tx, log.ChainId, reserve, k.reserveType(), "total_withdrawals", numOrZero(lp), log.BlockNumber)
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
		if err := adjustTokenBalance(tx, log.ChainId, reserve, asset, num(a, "amountIn"), log.BlockNumber); err != nil {
			return err
		}
		if err := adjustTokenBalance(tx, log.ChainId, reserve, asset, neg(num(a, "fee")), log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
			return err
		}
	}
	return bumpReserve(tx, log.ChainId, reserve, k.reserveType(), "total_deposits", numOrZero(lp), log.BlockNumber)
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
		if err := adjustTokenBalance(tx, log.ChainId, reserve, asset, neg(num(a, "amountOut")), log.BlockNumber); err != nil {
			return err
		}
		if err := adjustTokenBalance(tx, log.ChainId, reserve, asset, neg(num(a, "fee")), log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
			return err
		}
	}
	return bumpReserve(tx, log.ChainId, reserve, k.reserveType(), "total_withdrawals", numOrZero(lp), log.BlockNumber)
}

func handleRebalanced(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	if err := ensureReserve(tx, log.ChainId, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	if k == baseReserveKind {
		if err := adjustTokenBalance(tx, log.ChainId, reserve, normAddr(a["tokenIn"]), num(a, "amountIn"), log.BlockNumber); err != nil {
			return err
		}
		if err := adjustTokenBalance(tx, log.ChainId, reserve, normAddr(a["tokenOut"]), neg(num(a, "amountOut")), log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
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
	if err := ensureReserve(tx, log.ChainId, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	if k == baseReserveKind {
		// Principal is lent and returned within the tx; only the fee stays in the reserve.
		if err := adjustTokenBalance(tx, log.ChainId, reserve, normAddr(a["token"]), num(a, "fee"), log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
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
		if err := adjustTokenBalance(tx, log.ChainId, reserve, normAddr(a["asset"]), amount, log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
			return err
		}
	}
	return bumpReserve(tx, log.ChainId, reserve, k.reserveType(), "iou_minted", amount, log.BlockNumber)
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
		if err := adjustTokenBalance(tx, log.ChainId, reserve, normAddr(a["asset"]), neg(amount), log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
			return err
		}
	}
	return bumpReserve(tx, log.ChainId, reserve, k.reserveType(), "iou_redeemed", amount, log.BlockNumber)
}

func handleReserveSwap(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	if err := ensureReserve(tx, log.ChainId, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	if err := exec(tx, `INSERT INTO clear_reserve_swaps
(id, chain_id, reserve, block_number, log_index, tx_hash, trader, token_in, token_out, recipient,
 amount_in, amount_out, iou_total, trader_iou, treasury_iou, lp_iou)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT (id) DO NOTHING`,
		log.Id, log.ChainId, reserve, log.BlockNumber, log.LogIndex, log.TransactionHash,
		normAddr(a["trader"]), normAddr(a["tokenIn"]), normAddr(a["tokenOut"]), normAddr(a["recipient"]),
		num(a, "amountIn"), num(a, "amountOut"), num(a, "iouTotal"), num(a, "traderIOU"), num(a, "treasuryIOU"), num(a, "lpIOU")); err != nil {
		return err
	}
	if k == baseReserveKind {
		// amountIn of tokenIn received, amountOut of tokenOut paid out. The IOU
		// shortfall is a claim minted against NAV, not an underlying-token movement.
		if err := adjustTokenBalance(tx, log.ChainId, reserve, normAddr(a["tokenIn"]), num(a, "amountIn"), log.BlockNumber); err != nil {
			return err
		}
		if err := adjustTokenBalance(tx, log.ChainId, reserve, normAddr(a["tokenOut"]), neg(num(a, "amountOut")), log.BlockNumber); err != nil {
			return err
		}
		if err := snapshotReserveValue(tx, reserve, log); err != nil {
			return err
		}
	}
	return exec(tx, `UPDATE clear_reserves SET swap_count = swap_count + 1, last_block = $3 WHERE chain_id = $1 AND address = $2`,
		log.ChainId, reserve, log.BlockNumber)
}

func (e *clearExporter) handleAssetAdded(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	a := log.Args
	reserve := normAddr(log.Address)
	asset := normAddr(a["asset"])
	iou := normAddr(a["iou"])
	if err := ensureReserve(tx, log.ChainId, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	if iou != "" && !isZeroAddr(iou) {
		if err := ensureIou(tx, log.ChainId, iou, log.BlockNumber); err != nil {
			return err
		}
	}

	// The asset and the IOU minted against it both enter the schema here. The
	// event already states the asset's decimals, so that one field never needs a
	// getter even when the token does not implement decimals().
	if err := e.ensureToken(tx, log.ChainId, asset, log.BlockNumber, tokenMeta{decimals: nullInt(a, "decimals")}); err != nil {
		return err
	}
	if err := e.ensureToken(tx, log.ChainId, iou, log.BlockNumber, tokenMeta{}); err != nil {
		return err
	}
	if err := e.ensureToken(tx, log.ChainId, reserve, log.BlockNumber, tokenMeta{}); err != nil {
		return err
	}
	// position = current asset count for the reserve, so it mirrors the contract's
	// append-only assetList index; kept unchanged on a re-emit (assets are added once).
	//
	// `weight` is emitted by the META reserve only: each leg's target weight in bps
	// (int256 — BaseLP = targetBaseLpBps, native = its complement), fixed at
	// initialize. It is NULL for a base reserve, which values its assets at par and
	// has no target weights.
	return exec(tx, `INSERT INTO clear_reserve_assets (chain_id, reserve, asset, decimals, iou, position, weight)
VALUES ($1, $2, $3, $4, $5, (SELECT count(*) FROM clear_reserve_assets WHERE chain_id = $1 AND reserve = $2), $6)
ON CONFLICT (chain_id, reserve, asset) DO UPDATE SET
  decimals = EXCLUDED.decimals, iou = EXCLUDED.iou, weight = EXCLUDED.weight`,
		log.ChainId, reserve, asset, nullNum(a, "decimals"), nullEmpty(iou), nullNum(a, "weight"))
}

// --- curve pool events ---

func handleCurveSwap(tx *sql.Tx, log exporter.LogEvent, underlying bool) error {
	a := log.Args
	pool := normAddr(log.Address)
	if err := ensureCurve(tx, log.ChainId, pool, log.BlockNumber); err != nil {
		return err
	}
	if err := exec(tx, `INSERT INTO clear_curve_swaps
(id, chain_id, pool, block_number, log_index, tx_hash, buyer, sold_id, tokens_sold, bought_id, tokens_bought, underlying)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (id) DO NOTHING`,
		log.Id, log.ChainId, pool, log.BlockNumber, log.LogIndex, log.TransactionHash,
		normAddr(a["buyer"]), num(a, "sold_id"), num(a, "tokens_sold"),
		num(a, "bought_id"), num(a, "tokens_bought"), underlying); err != nil {
		return err
	}
	return exec(tx, `UPDATE clear_curve_pools SET swap_count = swap_count + 1, last_block = $3 WHERE chain_id = $1 AND address = $2`,
		log.ChainId, pool, log.BlockNumber)
}

func handleCurveLiquidity(tx *sql.Tx, log exporter.LogEvent, kind string) error {
	a := log.Args
	pool := normAddr(log.Address)
	if err := ensureCurve(tx, log.ChainId, pool, log.BlockNumber); err != nil {
		return err
	}
	tokenAmounts, err := jsonArrayArg(a, "token_amounts", false)
	if err != nil {
		return err
	}
	fees, err := jsonArrayArg(a, "fees", false)
	if err != nil {
		return err
	}
	return exec(tx, `INSERT INTO clear_curve_liquidity
(id, chain_id, pool, block_number, log_index, tx_hash, provider, kind,
 token_amounts, fees, token_id, token_amount, coin_amount, invariant, token_supply)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (id) DO NOTHING`,
		log.Id, log.ChainId, pool, log.BlockNumber, log.LogIndex, log.TransactionHash,
		normAddr(a["provider"]), kind,
		tokenAmounts, fees,
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
(chain_id, asset, oracle, enabled, asset_decimals, oracle_decimals, price_ttl, redemption_price, last_refresh, last_block)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (chain_id, asset) DO UPDATE SET
  oracle = EXCLUDED.oracle,
  enabled = EXCLUDED.enabled,
  asset_decimals = EXCLUDED.asset_decimals,
  oracle_decimals = EXCLUDED.oracle_decimals,
  price_ttl = EXCLUDED.price_ttl,
  redemption_price = EXCLUDED.redemption_price,
  last_refresh = EXCLUDED.last_refresh,
  last_block = EXCLUDED.last_block`,
		log.ChainId, asset, normAddr(log.Address), parseBool(a["enabled"]),
		nullNum(a, "assetDecimals"), nullNum(a, "oracleDecimals"),
		nullNum(a, "priceTTL"), nullNum(a, "redemptionPrice"), log.BlockTimestamp, log.BlockNumber)
}

func handleOracleRate(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	asset := normAddr(a["asset"])
	if asset == "" {
		return nil
	}
	if err := exec(tx, `INSERT INTO clear_oracle_prices (chain_id, asset, oracle, price, last_refresh, last_block)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (chain_id, asset) DO UPDATE SET
  oracle = EXCLUDED.oracle, price = EXCLUDED.price,
  last_refresh = EXCLUDED.last_refresh, last_block = EXCLUDED.last_block`,
		log.ChainId, asset, normAddr(log.Address), num(a, "price"), log.BlockTimestamp, log.BlockNumber); err != nil {
		return err
	}
	// Append the price point for charting (every price write emits this event).
	return exec(tx, `INSERT INTO clear_oracle_price_history (id, chain_id, asset, block_number, block_timestamp, price)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`,
		log.Id, log.ChainId, asset, log.BlockNumber, log.BlockTimestamp, num(a, "price"))
}

func handleOracleRedemption(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	asset := normAddr(a["asset"])
	if asset == "" {
		return nil
	}
	return exec(tx, `INSERT INTO clear_oracle_prices (chain_id, asset, redemption_price, last_refresh, last_block)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (chain_id, asset) DO UPDATE SET
  redemption_price = EXCLUDED.redemption_price,
  last_refresh = EXCLUDED.last_refresh, last_block = EXCLUDED.last_block`,
		log.ChainId, asset, num(a, "redemptionPrice"), log.BlockTimestamp, log.BlockNumber)
}

// handleOraclePublish records the adapter's price refresh. It does not overwrite
// `oracle` — log.Address here is the adapter, not the ClearOracle. `publishTime`
// is Pyth's own timestamp for the price (when the feed published it, which is
// older than the block that pushed it on-chain), kept alongside last_refresh so
// staleness can be judged against the source rather than against inclusion.
func handleOraclePublish(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	asset := normAddr(a["asset"])
	if asset == "" {
		return nil
	}
	return exec(tx, `INSERT INTO clear_oracle_prices (chain_id, asset, price, publish_time, last_refresh, last_block)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (chain_id, asset) DO UPDATE SET
  price = EXCLUDED.price, publish_time = EXCLUDED.publish_time,
  last_refresh = EXCLUDED.last_refresh, last_block = EXCLUDED.last_block`,
		log.ChainId, asset, num(a, "price"), nullNum(a, "publishTime"), log.BlockTimestamp, log.BlockNumber)
}

// --- reserve governance settings ---
//
// Every set* function on a reserve emits its own event (see RESERVE-SETTINGS.md in
// the contracts repo). They all fold into one row per reserve in
// clear_reserve_settings, each event patching only the column(s) it carries, so the
// row is created or updated in any order and always holds the latest value of each
// parameter. Parameters fixed at initialize (targetBaseLpBps, the asset set) are not
// emitted as settings events — the meta legs' target weights come through
// AssetAdded instead (clear_reserve_assets.weight).

// handleReserveSettings upserts the reserve's settings row. colVals is a flat list
// of column/value pairs (column names are code constants, never event data).
func handleReserveSettings(tx *sql.Tx, log exporter.LogEvent, k contractKind, colVals ...string) error {
	if len(colVals) == 0 || len(colVals)%2 != 0 {
		return fmt.Errorf("reserve settings %s: malformed column/value list", log.EventName)
	}
	reserve := normAddr(log.Address)
	if err := ensureReserve(tx, log.ChainId, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}

	cols := []string{"chain_id", "reserve", "kind"}
	args := []any{log.ChainId, reserve, k.reserveType()}
	for i := 0; i < len(colVals); i += 2 {
		cols = append(cols, colVals[i])
		args = append(args, colVals[i+1])
	}
	cols = append(cols, "last_block")
	args = append(args, log.BlockNumber)

	ph := make([]string, len(cols))
	for i := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	// Only the event's own columns (and last_block) are refreshed on conflict; the
	// key columns and every other setting keep their stored value.
	sets := make([]string, 0, len(cols)-3)
	for _, c := range cols[3:] {
		sets = append(sets, c+" = EXCLUDED."+c)
	}
	return exec(tx, fmt.Sprintf(`INSERT INTO clear_reserve_settings (%s) VALUES (%s)
ON CONFLICT (chain_id, reserve) DO UPDATE SET %s`,
		strings.Join(cols, ", "), strings.Join(ph, ", "), strings.Join(sets, ", ")), args...)
}

// --- IOU treasury fees ---

// handleIouTreasuryFee accumulates the treasury's cut of an IOU mint. The paired
// Transfer events already move supply and balances; ClearIOUMinted is the only
// place the fee split (user amount vs treasuryFee) is visible, so it is summed
// per IOU token.
func handleIouTreasuryFee(tx *sql.Tx, log exporter.LogEvent) error {
	token := normAddr(log.Address)
	if err := ensureIou(tx, log.ChainId, token, log.BlockNumber); err != nil {
		return err
	}
	fee := num(log.Args, "treasuryFee")
	if fee == "0" {
		return nil
	}
	return exec(tx, `UPDATE clear_iou_tokens SET treasury_fees = treasury_fees + $3, last_block = $4
WHERE chain_id = $1 AND address = $2`, log.ChainId, token, fee, log.BlockNumber)
}

// --- reserve factory (ClearReserveFactory) ---

// reserveKindFromType decodes the NewClearReserve `reserveType` enum
// (0 = BASE_RESERVE, 1 = META_RESERVE), tolerating both the numeric form evmi
// emits for a uint8 enum and the symbolic spellings.
func reserveKindFromType(s string) contractKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "base_reserve", "basereserve", "base":
		return baseReserveKind
	case "1", "meta_reserve", "metareserve", "meta":
		return metaReserveKind
	}
	return unknownKind
}

// handleNewClearReserve records a reserve at its creation and registers it in the
// contract registry with the kind the factory announced — so the reserve is routed
// correctly from its very first event without being listed in pluginConfig.
// (The reserve's logs still have to be delivered: evmi needs a pipeline source for
// that address, since one FACTORY source can only spawn children of a single ABI
// and the factory deploys two different ones.)
func (e *clearExporter) handleNewClearReserve(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	reserve := normAddr(a["reserve"])
	if reserve == "" || isZeroAddr(reserve) {
		return nil
	}
	k := reserveKindFromType(a["reserveType"])
	if k == unknownKind {
		return fmt.Errorf("NewClearReserve %s: unrecognized reserveType %q", log.Id, a["reserveType"])
	}
	if err := ensureReserve(tx, log.ChainId, reserve, k.reserveType(), log.BlockNumber); err != nil {
		return err
	}
	// `tokens` is an address[]; jsonArrayArg normalizes whichever rendering the
	// server used into a JSON array for the JSONB column, lowercased to match every
	// other stored address.
	tokens, err := jsonArrayArg(a, "tokens", true)
	if err != nil {
		return err
	}
	// The reserve's LP token is the reserve itself, and the factory names it here;
	// its underlying token set enters the schema at the same time.
	if err := e.ensureToken(tx, log.ChainId, reserve, log.BlockNumber,
		tokenMeta{name: a["name"], symbol: a["symbol"]}); err != nil {
		return err
	}
	reserveTokens, err := splitArrayArg(a["tokens"])
	if err != nil {
		return fmt.Errorf("NewClearReserve %s tokens: %w", log.Id, err)
	}
	if err := e.ensureTokens(tx, log.ChainId, reserveTokens, log.BlockNumber); err != nil {
		return err
	}
	if err := exec(tx, `UPDATE clear_reserves
SET name = $3, symbol = $4, implementation = $5, reserve_index = $6, tokens = $7, factory = $8
WHERE chain_id = $1 AND address = $2`,
		log.ChainId, reserve, nullEmpty(a["name"]), nullEmpty(a["symbol"]),
		nullEmpty(normAddr(a["implementation"])), nullNum(a, "index"), tokens,
		normAddr(log.Address)); err != nil {
		return err
	}
	return e.trackContract(tx, log.ChainId, reserve, k, log.BlockNumber)
}

// handleFactoryConfig patches one column of the factory's protocol-config row
// (the factory IS the IClearReserveConfig every reserve points at).
func handleFactoryConfig(tx *sql.Tx, log exporter.LogEvent, col, value string) error {
	if value == "" {
		return nil
	}
	return exec(tx, fmt.Sprintf(`INSERT INTO clear_protocol_config (chain_id, factory, %s, last_block)
VALUES ($1, $2, $3, $4)
ON CONFLICT (chain_id, factory) DO UPDATE SET %s = EXCLUDED.%s, last_block = EXCLUDED.last_block`,
		col, col, col), log.ChainId, normAddr(log.Address), value, log.BlockNumber)
}

// handleFactoryImplementation records an implementation upgrade (address + the
// version the factory bumped it to). Affects future deployments only.
func handleFactoryImplementation(tx *sql.Tx, log exporter.LogEvent, implCol, versionCol string) error {
	impl := normAddr(log.Args["implementation"])
	if impl == "" || isZeroAddr(impl) {
		return nil
	}
	return exec(tx, fmt.Sprintf(`INSERT INTO clear_protocol_config (chain_id, factory, %s, %s, last_block)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (chain_id, factory) DO UPDATE SET
  %s = EXCLUDED.%s, %s = EXCLUDED.%s, last_block = EXCLUDED.last_block`,
		implCol, versionCol, implCol, implCol, versionCol, versionCol),
		log.ChainId, normAddr(log.Address), impl, nullNum(log.Args, "version"), log.BlockNumber)
}

// --- curve pool deployer (ClearCurvePoolDeployer) ---

// handlePoolDeployed records a Curve pool at its creation, links it back to the
// reserve it was deployed for, and registers it as a tracked curve pool so its own
// events route to dispatchCurve. `coin` is the zero address on the reserve's plain
// base pool (there `pool == basePool`); on a metapool it is the coin paired against
// the base pool — an asset's IOU, or a meta reserve's native token / native IOU.
func (e *clearExporter) handlePoolDeployed(tx *sql.Tx, log exporter.LogEvent) error {
	a := log.Args
	pool := normAddr(a["pool"])
	if pool == "" || isZeroAddr(pool) {
		return nil
	}
	if err := ensureCurve(tx, log.ChainId, pool, log.BlockNumber); err != nil {
		return err
	}
	coin := normAddr(a["coin"])
	isBase := coin == "" || isZeroAddr(coin)
	if isBase {
		coin = ""
	}
	if err := exec(tx, `UPDATE clear_curve_pools
SET reserve = $3, base_pool = $4, coin = $5, is_base_pool = $6, deployer = $7
WHERE chain_id = $1 AND address = $2`,
		log.ChainId, pool, nullEmpty(normAddr(a["reserve"])), nullEmpty(normAddr(a["basePool"])),
		nullEmpty(coin), isBase, normAddr(log.Address)); err != nil {
		return err
	}
	// The pool's LP token IS the pool, and `coin` is the asset the metapool pairs.
	if err := e.ensureTokens(tx, log.ChainId, []string{pool, coin}, log.BlockNumber); err != nil {
		return err
	}
	return e.trackContract(tx, log.ChainId, pool, curveKind, log.BlockNumber)
}

// --- curve stableswap-ng factory (CurveStableSwapFactoryNG) ---

// handleCurvePoolDeployed records a pool the Curve factory just created. Neither
// PlainPoolDeployed nor MetaPoolDeployed names the new pool, so its address is
// resolved from the deployment transaction over the chain's own RPC endpoint
// (resolveDeployedPool), the metadata that never reaches a log is read back with
// a handful of getters (fetchCurvePool), and the pool is registered with EVMI as
// a child log source of the factory — which is what makes its own events (swaps,
// liquidity, LP transfers) start being delivered to dispatchCurve.
//
// It writes only its own columns: a pool built by the ClearCurvePoolDeployer gets
// PoolDeployed in the same transaction, and the reserve linkage that event carries
// must survive whichever of the two lands second.
//
// Without an RPC endpoint (no host API and no rpcUrl in the config) the plugin
// degrades to a warning: the event is still consumed, the pool simply is not
// picked up — the pre-host-API behaviour, where pools were declared by hand.
func (e *clearExporter) handleCurvePoolDeployed(tx *sql.Tx, log exporter.LogEvent, meta bool) error {
	a := log.Args
	factory := normAddr(log.Address)

	if e.rpc == nil {
		warnf("[%s] %s %s: no rpc endpoint (host api unavailable and rpcUrl unset), pool not indexed", e.name, log.EventName, log.Id)
		return nil
	}

	// A transport error is worth retrying, so it fails the event (the tx rolls
	// back, the processed-events claim with it, and EVMI redelivers from this
	// block). A receipt that simply holds no candidate is not: warn and move on.
	pool, err := resolveDeployedPool(e.rpc, factory, log.TransactionHash, log.LogIndex)
	if err != nil {
		return fmt.Errorf("resolve deployed pool from tx %s: %w", log.TransactionHash, err)
	}
	if pool == "" {
		warnf("[%s] %s %s: no pool constructor log in tx %s, pool not indexed", e.name, log.EventName, log.Id, log.TransactionHash)
		return nil
	}

	info := fetchCurvePool(e.rpc, factory, pool)
	coins := info.coins
	if len(coins) == 0 {
		if coins, err = eventCoins(a, meta); err != nil {
			return fmt.Errorf("%s coins: %w", log.EventName, err)
		}
	}
	coinsJSON, err := jsonStrings(coins)
	if err != nil {
		return err
	}

	isMeta := meta
	if info.isMeta.Valid {
		isMeta = info.isMeta.Bool
	}
	var nCoins sql.NullInt64
	if len(coins) > 0 {
		nCoins = sql.NullInt64{Int64: int64(len(coins)), Valid: true}
	}
	// The amplification coefficient is spelled `A` in the Vyper event; accept the
	// lowercase spelling too in case a hand-written ABI normalizes it.
	amp := nullNum(a, "A")
	if !amp.Valid {
		amp = nullNum(a, "a")
	}

	if err := ensureCurve(tx, log.ChainId, pool, log.BlockNumber); err != nil {
		return err
	}
	if err := exec(tx, `UPDATE clear_curve_pools SET
  curve_factory  = $3,
  is_meta        = $4,
  amplification  = $5,
  fee            = $6,
  name           = COALESCE(NULLIF($7::text, ''), name),
  symbol         = COALESCE(NULLIF($8::text, ''), symbol),
  decimals       = COALESCE($9::smallint, decimals),
  implementation = COALESCE(NULLIF($10::text, ''), implementation),
  coins          = COALESCE($11::jsonb, coins),
  n_coins        = COALESCE($12::smallint, n_coins),
  deployer       = COALESCE(deployer, NULLIF($13::text, '')),
  base_pool      = COALESCE(base_pool, NULLIF($14::text, '')),
  coin           = COALESCE(coin, NULLIF($15::text, ''))
WHERE chain_id = $1 AND address = $2`,
		log.ChainId, pool, factory, isMeta,
		amp, nullNum(a, "fee"),
		info.name, info.symbol, info.decimals, info.implementation,
		coinsJSON, nCoins,
		normAddr(a["deployer"]), normAddr(a["base_pool"]), normAddr(a["coin"])); err != nil {
		return err
	}

	// The pool's LP token IS the pool, and its metadata was just read back, so
	// this costs no extra RPC. The coins it pairs enter the schema here too.
	if err := e.ensureToken(tx, log.ChainId, pool, log.BlockNumber,
		tokenMeta{name: info.name, symbol: info.symbol, decimals: info.decimals}); err != nil {
		return err
	}
	if err := e.ensureTokens(tx, log.ChainId, coins, log.BlockNumber); err != nil {
		return err
	}

	// The coin list is fixed at deployment, so the upsert is only there to make a
	// redelivery a no-op.
	for i, coin := range coins {
		if err := exec(tx, `INSERT INTO clear_curve_pool_coins (chain_id, pool, position, coin, decimals)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (chain_id, pool, position) DO UPDATE
SET coin = EXCLUDED.coin, decimals = COALESCE(EXCLUDED.decimals, clear_curve_pool_coins.decimals)`,
			log.ChainId, pool, i, coin, coinDecimals(info.coinDecimals, i)); err != nil {
			return err
		}
	}

	if err := e.trackContract(tx, log.ChainId, pool, curveKind, log.BlockNumber); err != nil {
		return err
	}
	return e.registerCurvePoolSource(log, pool)
}
