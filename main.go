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
// All state is derived purely from events, so balances are exact only if indexing
// starts at or before each contract's first event. Delivery is at-least-once; a
// processed-event ledger makes it exactly-once.
//
// The one thing events cannot supply is the address of a pool the Curve factory
// just deployed — PlainPoolDeployed/MetaPoolDeployed do not carry it — so that one
// path uses the host API: the chain's RPC endpoint to resolve the pool from its
// deployment transaction, and CreateLogSource to have EVMI index it. See curve.go
// and host.go; without a host/RPC it degrades to a warning.
//
// Routing is BY ADDRESS. On Init the plugin loads a contract registry
// (address -> kind) from pluginConfig.contracts and from the clear_contracts
// table; each log is routed to a per-contract dispatcher by looking its address up
// in that registry, instead of re-classifying the ABI name on every event. New
// contracts (a factory-spawned IOU at AssetAdded, or any address seen for the
// first time) are registered dynamically and persisted, so the registry grows as
// the protocol does and is restored on restart. See registry.go / dispatch.go.
//
// The plugin is an ordinary Go program: EVMI launches it as a SUBPROCESS and
// talks to it over gRPC (hashicorp/go-plugin), so it is built with a plain
// `go build` and its toolchain / dependency versions are its own business.
//
//	go build -o clear-defi .
//
// NEVER write to stdout: stdout carries the go-plugin handshake and the gRPC
// stream. Log to stderr (the standard library `log` package already does) —
// EVMI captures it and forwards it to its own log.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	exporter "github.com/evmi-cloud/go-evm-indexer/pkg/exporter"
	_ "github.com/lib/pq"
	"github.com/lmittmann/w3"
)

type pluginConfig struct {
	// Dsn is the PostgreSQL connection string (lib/pq format or URL).
	Dsn string `json:"dsn"`
	// AutoMigrate creates the tables on start (default true).
	AutoMigrate *bool `json:"autoMigrate"`
	// Contracts is the initial list of contracts to track, loaded into the
	// address->kind registry on Init. IOU tokens are usually discovered
	// dynamically at AssetAdded, so they need not be listed here.
	Contracts []contractConfig `json:"contracts"`
	// RpcUrl overrides the chain endpoint used to resolve a Curve pool address off
	// a factory deployment. Empty (the normal case) means the endpoint EVMI itself
	// polls, obtained from Host.Blockchain().
	RpcUrl string `json:"rpcUrl"`
	// CurvePoolAbi is the contract name of the ABI that log sources created for
	// discovered pools decode with (default CurveStableSwapNG). It is registered
	// from the ABI embedded in this plugin if the server does not have it.
	CurvePoolAbi string `json:"curvePoolAbi"`
	// IndexCurvePools registers a discovered pool with EVMI as a new log source
	// (default true). Turning it off keeps the pool's row in clear_curve_pools but
	// leaves indexing it to a hand-declared source.
	IndexCurvePools *bool `json:"indexCurvePools"`
}

// contractConfig is one entry of pluginConfig.contracts: an address and its kind
// (base_reserve | meta_reserve | iou | curve | oracle). See kindFromString.
type contractConfig struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
}

type clearExporter struct {
	name    string
	db      *sql.DB
	chainID uint64

	// registry maps a normalized contract address to its kind for this chain. It
	// is loaded on Init and extended at runtime as new contracts are discovered.
	mu       sync.RWMutex
	registry map[string]contractKind

	// host is the reverse channel into EVMI (nil on a server older than the host
	// API), and rpc a w3 client on the chain endpoint it hands out — both used
	// only on a Curve factory deployment, whose event does not name the pool it
	// created.
	host exporter.Host
	rpc  *w3.Client

	// indexCurvePools registers a discovered pool as a new log source;
	// curvePoolAbi is the ABI those sources decode with, its id resolved once and
	// cached under abiMu.
	indexCurvePools bool
	curvePoolAbi    string
	abiMu           sync.Mutex
	curveAbiID      uint64
}

var (
	_ exporter.Exporter     = (*clearExporter)(nil)
	_ exporter.Configurable = (*clearExporter)(nil)
)

func (e *clearExporter) Name() string { return "clear-defi" }

// ConfigSchema implements the optional exporter.Configurable interface: EVMI
// extracts it once at install time (by running this binary) and validates every
// exporter's pluginConfig against it before starting.
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
		{
			Name:        "rpcUrl",
			Type:        exporter.StringField,
			Required:    false,
			Description: "Chain RPC endpoint used to resolve Curve pool deployments; defaults to the one EVMI polls",
		},
		{
			Name:        "curvePoolAbi",
			Type:        exporter.StringField,
			Required:    false,
			Description: "Contract name of the ABI that log sources created for discovered Curve pools decode with",
			Default:     defaultCurvePoolAbiName,
		},
		{
			Name:        "indexCurvePools",
			Type:        exporter.BoolField,
			Required:    false,
			Description: "Register a Curve pool discovered on PlainPoolDeployed/MetaPoolDeployed as a new log source",
			Default:     "true",
		},
		// `contracts` is a JSON array of {address, kind} and can't be expressed as a
		// scalar ConfigField; EVMI allows unknown extra keys, so it is decoded from
		// the raw config JSON (see pluginConfig) rather than declared here.
	}
}

func (e *clearExporter) Init(ctx exporter.Context) error {
	e.name = ctx.ExporterName
	e.chainID = ctx.ChainId
	e.registry = map[string]contractKind{}

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

	// Seed the registry from config, then (re)load every tracked contract for this
	// chain — including any discovered dynamically on previous runs.
	if err := e.seedContracts(cfg.Contracts); err != nil {
		return fmt.Errorf("seed contracts: %w", err)
	}
	if err := e.loadRegistry(); err != nil {
		return fmt.Errorf("load registry: %w", err)
	}

	// The host API is only needed on a Curve factory deployment, so an old server
	// (Host == nil) or an endpoint that cannot be read is a warning, not a fatal
	// error: everything else the plugin does is derived from the events alone.
	e.host = ctx.Host
	e.curvePoolAbi = strings.TrimSpace(cfg.CurvePoolAbi)
	if e.curvePoolAbi == "" {
		e.curvePoolAbi = defaultCurvePoolAbiName
	}
	e.indexCurvePools = cfg.IndexCurvePools == nil || *cfg.IndexCurvePools

	rpcURL := strings.TrimSpace(cfg.RpcUrl)
	if rpcURL == "" && ctx.Host != nil {
		chain, err := ctx.Host.Blockchain()
		if err != nil {
			log.Printf("[%s] host blockchain: %v — curve pool discovery disabled", e.name, err)
		} else {
			rpcURL = strings.TrimSpace(chain.RpcUrl)
		}
	}
	if rpcURL != "" {
		// http/https/ws/wss, or a path for an IPC socket. Dial is lazy over HTTP,
		// so this does not fail Init when the node is briefly unreachable.
		client, err := w3.Dial(rpcURL)
		if err != nil {
			log.Printf("[%s] dial rpc: %v — curve pool discovery disabled", e.name, err)
		} else {
			e.rpc = client
		}
	}
	if e.rpc == nil {
		log.Printf("[%s] no rpc endpoint — curve pools deployed through the curve factory will not be indexed", e.name)
	}

	// stderr: stdout carries the go-plugin handshake and the gRPC stream.
	log.Printf("[%s] init pipeline=%d chain=%d contracts=%d host=%t rpc=%t",
		e.name, ctx.PipelineId, ctx.ChainId, len(e.registry), e.host != nil, e.rpc != nil)
	return nil
}

// NewLogEvent processes one log inside a transaction. It first claims the event
// id in the processed ledger; if the id is already present (a redelivery after a
// restart) it does nothing, so balance arithmetic is applied exactly once. The
// contract kind is resolved by address from the registry (discovering and
// persisting a new contract on first sight) and the log is routed to its
// per-contract dispatcher.
func (e *clearExporter) NewLogEvent(log exporter.LogEvent) error {
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}

	res, err := tx.Exec(`INSERT INTO clear_processed_events (id, chain_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, log.Id, log.ChainId)
	if err != nil {
		tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Rollback() // already processed
	}

	k, err := e.resolveKind(tx, log)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("resolve contract %s (%s): %w", log.Address, log.Id, err)
	}
	if k == unknownKind {
		// Log from an untracked contract: nothing to materialize. The id stays
		// claimed so we don't revisit it.
		return tx.Commit()
	}

	if err := e.dispatch(tx, log, k); err != nil {
		tx.Rollback()
		return fmt.Errorf("event %s (%s): %w", log.EventName, log.Id, err)
	}
	return tx.Commit()
}

func (e *clearExporter) Close() error {
	if e.rpc != nil {
		e.rpc.Close()
	}
	if e.db != nil {
		return e.db.Close()
	}
	return nil
}

// main hands the exporter to the SDK's go-plugin server. Serve blocks until EVMI
// closes the connection (exporter stopped / server shutdown), then returns — all
// setup belongs in Init, not here.
func main() { exporter.Serve(&clearExporter{}) }
