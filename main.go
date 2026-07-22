// EVMI exporter plugin for the Clear DeFi protocol (github.com/…/clear-smart-contracts).
//
// It consumes the pipeline's decoded logs in block order and materializes the
// protocol state into PostgreSQL:
//
//   - reserves (base & meta) — LP supply, cumulative deposits/withdrawals, IOU
//     minted/redeemed, swap counts, and per-holder LP balances;
//   - IOU tokens — supply and per-holder balances;
//   - Curve StableSwap-NG pools — LP supply, per-holder LP balances, swap counts;
//   - full history of reserve depeg swaps and Curve swaps, plus reserve activity
//     (deposits/withdrawals/rebalances/IOU) and Curve liquidity events.
//
// All state is derived purely from events (the plugin gets no RPC access), so
// balances are exact only if indexing starts at or before each contract's first
// event. Delivery is at-least-once; a processed-event ledger makes it exactly-once.
//
// Build (with the SAME toolchain/module versions as the evmi server):
//
//	go build -buildmode=plugin -o clear-defi.so ./examples/exporters/clear-defi
//
// Contract classification is by ABI ContractName (case-insensitive substring):
// name the source ABIs so they contain Base/Meta + Reserve, IOU, or Curve /
// StableSwap / Pool.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"

	exporter "github.com/evmi-cloud/go-evm-indexer/pkg/exporter"
	_ "github.com/lib/pq"
)

type pluginConfig struct {
	// Dsn is the PostgreSQL connection string (lib/pq format or URL).
	Dsn string `json:"dsn"`
	// AutoMigrate creates the tables on start (default true).
	AutoMigrate *bool `json:"autoMigrate"`
}

type clearExporter struct {
	name string
	db   *sql.DB
}

func (e *clearExporter) Name() string { return "clear-defi" }

func (e *clearExporter) ConfigSchema() []exporter.ConfigField {
	return []exporter.ConfigField{
		{
			Name:        "dsn",
			Type:        exporter.StringField,
			Required:    true,
			Description: "PostgreSQL DSN, e.g. postgres://user:pass@host:5432/clear?sslmode=disable",
		},
		{
			Name:        "autoMigrate",
			Type:        exporter.BoolField,
			Required:    false,
			Description: "Create the schema on start",
			Default:     "true",
		},
	}
}

func (e *clearExporter) Init(ctx exporter.Context) error {
	e.name = ctx.ExporterName

	var cfg pluginConfig
	if len(ctx.Config) > 0 {
		if err := json.Unmarshal(ctx.Config, &cfg); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}
	}
	if cfg.Dsn == "" {
		return fmt.Errorf("dsn is required")
	}

	db, err := sql.Open("postgres", cfg.Dsn)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return fmt.Errorf("ping postgres: %w", err)
	}
	e.db = db

	if cfg.AutoMigrate == nil || *cfg.AutoMigrate {
		if _, err := db.Exec(schema); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	fmt.Printf("[%s] init pipeline=%d chain=%d\n", e.name, ctx.PipelineId, ctx.ChainId)
	return nil
}

// NewLogEvent processes one log inside a transaction. It first claims the event
// id in the processed ledger; if the id is already present (a redelivery after a
// restart) it does nothing, so balance arithmetic is applied exactly once.
func (e *clearExporter) NewLogEvent(log exporter.LogEvent) error {
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}

	res, err := tx.Exec(`INSERT INTO clear_processed_events (id) VALUES ($1) ON CONFLICT DO NOTHING`, log.Id)
	if err != nil {
		tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Rollback() // already processed
	}

	if err := dispatch(tx, log); err != nil {
		tx.Rollback()
		return fmt.Errorf("event %s (%s): %w", log.EventName, log.Id, err)
	}
	return tx.Commit()
}

func (e *clearExporter) Close() error {
	if e.db != nil {
		return e.db.Close()
	}
	return nil
}

// dispatch routes one decoded log to its handler. Unhandled events (Approval,
// config/oracle updates, …) are ignored.
func dispatch(tx *sql.Tx, log exporter.LogEvent) error {
	k := classify(log.ContractName)
	switch log.EventName {
	// ERC20 balances (reserve LP / IOU / curve LP), routed by contract kind.
	case "Transfer":
		return handleTransfer(tx, log, k)

	// Reserve activity.
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
		return handleAssetAdded(tx, log, k)
	case "FlashLoan":
		return handleFlashLoan(tx, log, k)

	// Curve pool activity.
	case "TokenExchange":
		return handleCurveSwap(tx, log, false)
	case "TokenExchangeUnderlying":
		return handleCurveSwap(tx, log, true)
	// Oracle state (per-asset config, price, redemption, refresh time).
	case "OracleConfigured":
		return handleOracleConfigured(tx, log)
	case "ClearOracleRateChanged":
		return handleOracleRate(tx, log)
	case "ClearOracleRedemptionPriceChanged":
		return handleOracleRedemption(tx, log)
	case "PriceUpdated":
		return handleOraclePublish(tx, log)

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

// New is the symbol the EVMI server looks up to instantiate the plugin.
func New() exporter.Exporter { return &clearExporter{} }

// main is required for -buildmode=plugin but never executed.
func main() {}
