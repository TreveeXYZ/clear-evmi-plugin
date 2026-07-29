package main

import (
	"database/sql"

	exporter "github.com/evmi-cloud/go-evm-indexer/pkg/exporter"
)

// dispatch routes a log to its per-contract dispatcher using the kind already
// resolved by address (see resolveKind). Each dispatcher handles only the events
// its contract emits, so the routing logic is split per contract rather than one
// flat switch that re-derives the contract type for every event.
func (e *clearExporter) dispatch(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	switch k {
	case baseReserveKind, metaReserveKind:
		return e.dispatchReserve(tx, log, k)
	case iouKind:
		return dispatchIOU(tx, log)
	case curveKind:
		return dispatchCurve(tx, log)
	case oracleKind:
		return dispatchOracle(tx, log)
	default:
		return nil
	}
}

// dispatchReserve handles Clear base/meta reserve events. On AssetAdded it also
// registers the announced IOU token as a tracked contract, so the IOU's own logs
// route to dispatchIOU from then on (the reserve is a factory for its IOUs).
func (e *clearExporter) dispatchReserve(tx *sql.Tx, log exporter.LogEvent, k contractKind) error {
	switch log.EventName {
	case "Transfer":
		return handleTransfer(tx, log, k)
	case "Deposit":
		return handleReserveDeposit(tx, log, k)
	case "Withdraw":
		return handleReserveWithdraw(tx, log, k)
	case "SingleAssetDeposit":
		return handleSingleAssetDeposit(tx, log, k)
	case "SingleAssetWithdraw":
		return handleSingleAssetWithdraw(tx, log, k)
	case "Rebalanced":
		return handleRebalanced(tx, log, k)
	case "Swap":
		return handleReserveSwap(tx, log, k)
	case "IOUMinted":
		return handleIOUMinted(tx, log, k)
	case "IOURedeemed":
		return handleIOURedeemed(tx, log, k)
	case "AssetAdded":
		if err := handleAssetAdded(tx, log, k); err != nil {
			return err
		}
		// Track the spawned IOU so its Transfers route to the IOU dispatcher.
		iou := normAddr(firstArg(log.Args, "iou"))
		if iou == "" || isZeroAddr(iou) {
			return nil
		}
		return e.trackContract(tx, log.ChainId, iou, iouKind, log.BlockNumber)
	case "FlashLoan":
		return handleFlashLoan(tx, log, k)
	}
	return nil
}

// dispatchIOU handles IOU token events (only ERC20 Transfer drives state; mint/
// burn markers are covered by the zero-address Transfer decomposition).
func dispatchIOU(tx *sql.Tx, log exporter.LogEvent) error {
	if log.EventName == "Transfer" {
		return handleTransfer(tx, log, iouKind)
	}
	return nil
}

// dispatchCurve handles Curve StableSwap-NG pool events.
func dispatchCurve(tx *sql.Tx, log exporter.LogEvent) error {
	switch log.EventName {
	case "Transfer":
		return handleTransfer(tx, log, curveKind)
	case "TokenExchange":
		return handleCurveSwap(tx, log, false)
	case "TokenExchangeUnderlying":
		return handleCurveSwap(tx, log, true)
	case "AddLiquidity":
		return handleCurveLiquidity(tx, log, "add")
	case "RemoveLiquidity":
		return handleCurveLiquidity(tx, log, "remove")
	case "RemoveLiquidityOne":
		return handleCurveLiquidity(tx, log, "remove_one")
	case "RemoveLiquidityImbalance":
		return handleCurveLiquidity(tx, log, "remove_imbalance")
	}
	return nil
}

// dispatchOracle handles ClearOracle and PythOracleAdapter events (both classify
// as oracleKind and fold into clear_oracle_prices by asset).
func dispatchOracle(tx *sql.Tx, log exporter.LogEvent) error {
	switch log.EventName {
	case "OracleConfigured":
		return handleOracleConfigured(tx, log)
	case "ClearOracleRateChanged":
		return handleOracleRate(tx, log)
	case "ClearOracleRedemptionPriceChanged":
		return handleOracleRedemption(tx, log)
	case "PriceUpdated":
		return handleOraclePublish(tx, log)
	}
	return nil
}
