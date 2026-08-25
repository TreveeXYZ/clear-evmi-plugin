package main

import (
	"database/sql"
	"strings"

	exporter "github.com/evmi-cloud/go-evm-indexer/pkg/exporter"
)

// The contract registry maps a normalized address to its contractKind so a log
// can be routed by ADDRESS in O(1), instead of re-classifying its ABI name on
// every event. It is backed by the clear_contracts table (source of truth across
// restarts) and mirrored in memory (clearExporter.registry).

// String is the stored/config token for a kind (the clear_contracts.kind column
// and pluginConfig.contracts[].kind). Distinct from reserveType(), which is the
// clear_reserves.kind value.
func (k contractKind) String() string {
	switch k {
	case baseReserveKind:
		return "base_reserve"
	case metaReserveKind:
		return "meta_reserve"
	case iouKind:
		return "iou"
	case curveKind:
		return "curve"
	case oracleKind:
		return "oracle"
	case factoryKind:
		return "factory"
	case curveDeployerKind:
		return "curve_deployer"
	case curveFactoryKind:
		return "curve_factory"
	default:
		return "unknown"
	}
}

// kindFromString parses a kind token from config or the clear_contracts table,
// tolerating a few spellings. Unrecognized -> unknownKind.
func kindFromString(s string) contractKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "base_reserve", "basereserve", "base":
		return baseReserveKind
	case "meta_reserve", "metareserve", "meta":
		return metaReserveKind
	case "iou":
		return iouKind
	case "curve", "pool", "stableswap", "stableswapng":
		return curveKind
	case "oracle":
		return oracleKind
	case "factory", "reserve_factory", "reservefactory":
		return factoryKind
	case "curve_deployer", "curvedeployer", "pool_deployer", "pooldeployer", "deployer":
		return curveDeployerKind
	case "curve_factory", "curvefactory", "stableswap_factory", "stableswapfactory", "pool_factory", "poolfactory":
		return curveFactoryKind
	default:
		return unknownKind
	}
}

// seedContracts upserts the configured contracts into clear_contracts for this
// chain (source='config', which wins over a prior 'dynamic' row). Entries with an
// empty address or unrecognized kind are skipped. Runs outside a per-event tx.
func (e *clearExporter) seedContracts(contracts []contractConfig) error {
	for _, c := range contracts {
		addr := normAddr(c.Address)
		k := kindFromString(c.Kind)
		if addr == "" || k == unknownKind {
			continue
		}
		if _, err := e.db.Exec(`INSERT INTO clear_contracts (chain_id, address, kind, source)
VALUES ($1, $2, $3, 'config')
ON CONFLICT (chain_id, address) DO UPDATE SET kind = EXCLUDED.kind, source = 'config'`,
			e.chainID, addr, k.String()); err != nil {
			return err
		}
	}
	return nil
}

// loadRegistry (re)builds the in-memory registry from clear_contracts for this
// chain, so contracts discovered on previous runs are restored on startup.
func (e *clearExporter) loadRegistry() error {
	rows, err := e.db.Query(`SELECT address, kind FROM clear_contracts WHERE chain_id = $1`, e.chainID)
	if err != nil {
		return err
	}
	defer rows.Close()

	reg := map[string]contractKind{}
	for rows.Next() {
		var addr, kind string
		if err := rows.Scan(&addr, &kind); err != nil {
			return err
		}
		if k := kindFromString(kind); k != unknownKind {
			reg[normAddr(addr)] = k
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	e.mu.Lock()
	e.registry = reg
	e.mu.Unlock()
	return nil
}

// resolveKind returns the kind of the log's contract, routing by address. On the
// first sight of an address it falls back to classifying the ABI name once, caches
// the result in memory, and (for a known kind) persists it as a dynamic contract —
// so classification happens at most once per address, never per event. The
// classify() result is cached even when unknown to avoid re-work.
func (e *clearExporter) resolveKind(tx *sql.Tx, log exporter.LogEvent) (contractKind, error) {
	addr := normAddr(log.Address)

	e.mu.RLock()
	k, ok := e.registry[addr]
	e.mu.RUnlock()
	if ok {
		return k, nil
	}

	k = classify(log.ContractName)
	e.rememberKind(addr, k)
	if k == unknownKind {
		return k, nil
	}
	if err := e.persistContract(tx, log.ChainId, addr, k, "dynamic", log.BlockNumber); err != nil {
		return k, err
	}
	return k, nil
}

// trackContract registers a contract discovered while handling an event (e.g. a
// factory-spawned IOU announced by AssetAdded): it updates the in-memory registry
// and persists the row inside the caller's tx. A no-op for an empty address or
// unknown kind.
func (e *clearExporter) trackContract(tx *sql.Tx, chainID uint64, addr string, k contractKind, block uint64) error {
	addr = normAddr(addr)
	if addr == "" || k == unknownKind {
		return nil
	}
	e.rememberKind(addr, k)
	return e.persistContract(tx, chainID, addr, k, "dynamic", block)
}

func (e *clearExporter) rememberKind(addr string, k contractKind) {
	e.mu.Lock()
	e.registry[normAddr(addr)] = k
	e.mu.Unlock()
}

// persistContract inserts a tracked contract, keeping the first writer's row
// (config seeding, which runs first, therefore wins over dynamic discovery).
func (e *clearExporter) persistContract(tx *sql.Tx, chainID uint64, addr string, k contractKind, source string, block uint64) error {
	var blk sql.NullInt64
	if block > 0 {
		blk = sql.NullInt64{Int64: int64(block), Valid: true}
	}
	_, err := tx.Exec(`INSERT INTO clear_contracts (chain_id, address, kind, source, first_block)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (chain_id, address) DO NOTHING`,
		chainID, normAddr(addr), k.String(), source, blk)
	return err
}
