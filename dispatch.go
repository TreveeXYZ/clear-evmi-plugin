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
	case factoryKind:
		return e.dispatchFactory(tx, log)
	case curveDeployerKind:
		return e.dispatchCurveDeployer(tx, log)
	case curveFactoryKind:
		return e.dispatchCurveFactory(tx, log)
	default:
		return nil
	}
}

// dispatchReserve handles Clear base/meta reserve events. On AssetAdded it also
// registers the announced IOU token as a tracked contract, so the IOU's own logs
// route to dispatchIOU from then on (the reserve is a factory for its IOUs). Both
// reserve types emit AssetAdded: the base reserve once per underlying asset, the
// meta reserve once per leg (native / BaseLP) with that leg's target `weight`.
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
		if err := e.handleAssetAdded(tx, log, k); err != nil {
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

	// --- governance settings (every set* function emits one of these) ---
	// Each folds its own column(s) into the reserve's clear_reserve_settings row.
	case "ConfigUpdated":
		return handleReserveSettings(tx, log, k, "config_address", normAddr(log.Args["config"]))
	case "FlashFeeUpdated":
		return handleReserveSettings(tx, log, k, "flash_fee_bps", num(log.Args, "flashFeeBps"))
	case "BaseQuoteFeeBpsUpdated":
		return handleReserveSettings(tx, log, k, "base_quote_fee_bps", num(log.Args, "baseQuoteFeeBps"))
	case "SingleAssetFeeBpsUpdated":
		return handleReserveSettings(tx, log, k, "single_asset_fee_bps", num(log.Args, "singleAssetFeeBps"))
	case "RedemptionProximityBpsUpdated":
		return handleReserveSettings(tx, log, k, "redemption_proximity_bps", num(log.Args, "redemptionProximityBps"))
	case "RebalanceTriggerBpsUpdated":
		return handleReserveSettings(tx, log, k, "rebalance_trigger_bps", num(log.Args, "triggerBps"))
	case "DepositWeightToleranceBpsUpdated":
		return handleReserveSettings(tx, log, k, "deposit_weight_tolerance_bps", num(log.Args, "toleranceBps"))
	case "IouDistributionUpdated":
		return handleReserveSettings(tx, log, k,
			"iou_trader_bps", num(log.Args, "traderBps"),
			"iou_treasury_bps", num(log.Args, "treasuryBps"))
	case "BaseQuoteDistributionUpdated":
		return handleReserveSettings(tx, log, k,
			"base_quote_trader_bps", num(log.Args, "traderBps"),
			"base_quote_treasury_bps", num(log.Args, "treasuryBps"))
	case "SwapSpreadBpsUpdated":
		return handleReserveSettings(tx, log, k,
			"swap_spread_min_bps", num(log.Args, "minBps"),
			"swap_spread_max_bps", num(log.Args, "maxBps"))
	}
	return nil
}

// dispatchIOU handles IOU token events. Supply and per-holder balances come from
// the zero-address Transfer decomposition; ClearIOUMinted adds the one figure the
// Transfer pair does not carry — how much of the mint went to the treasury as fee.
func dispatchIOU(tx *sql.Tx, log exporter.LogEvent) error {
	switch log.EventName {
	case "Transfer":
		return handleTransfer(tx, log, iouKind)
	case "ClearIOUMinted":
		return handleIouTreasuryFee(tx, log)
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

// dispatchFactory handles ClearReserveFactory events. NewClearReserve is the
// protocol's own discovery event: it names every reserve the factory deploys and
// whether it is a base or a meta reserve, so the reserve is registered in the
// contract registry from its creation log rather than having to be configured by
// hand (the reserve's own logs still need a pipeline source for that address).
func (e *clearExporter) dispatchFactory(tx *sql.Tx, log exporter.LogEvent) error {
	switch log.EventName {
	case "NewClearReserve":
		return e.handleNewClearReserve(tx, log)
	case "NewTreasury":
		return handleFactoryConfig(tx, log, "treasury", normAddr(log.Args["newTreasury"]))
	case "NewBaseReserveImplementation":
		return handleFactoryImplementation(tx, log, "base_reserve_implementation", "base_reserve_version")
	case "NewMetaReserveImplementation":
		return handleFactoryImplementation(tx, log, "meta_reserve_implementation", "meta_reserve_version")
	case "NewIOUImplementation":
		return handleFactoryImplementation(tx, log, "iou_implementation", "iou_version")
	}
	return nil
}

// dispatchCurveDeployer handles ClearCurvePoolDeployer events. PoolDeployed names
// every Curve pool deployed for a reserve, so pools are registered (and linked
// back to their reserve / base pool / paired coin) from their creation log.
func (e *clearExporter) dispatchCurveDeployer(tx *sql.Tx, log exporter.LogEvent) error {
	if log.EventName == "PoolDeployed" {
		return e.handlePoolDeployed(tx, log)
	}
	return nil
}

// dispatchCurveFactory handles CurveStableSwapFactoryNG events, which catch every
// pool deployed through the Curve factory — the ones ClearCurvePoolDeployer built
// and the ones it did not. Neither PlainPoolDeployed nor MetaPoolDeployed carries
// the address of the pool it created, so the handler resolves it over RPC and
// registers the pool as a new log source (see curve.go / host.go).
// BasePoolAdded and LiquidityGaugeDeployed are ignored.
func (e *clearExporter) dispatchCurveFactory(tx *sql.Tx, log exporter.LogEvent) error {
	switch log.EventName {
	case "PlainPoolDeployed":
		return e.handleCurvePoolDeployed(tx, log, false)
	case "MetaPoolDeployed":
		return e.handleCurvePoolDeployed(tx, log, true)
	}
	return nil
}
