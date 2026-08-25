package main

import (
	"fmt"
	"log"

	exporter "github.com/evmi-cloud/go-evm-indexer/pkg/exporter"
)

// Host API use (SDK >= 0.3.0). Context.Host is the reverse channel into EVMI, and
// the plugin uses exactly two things from it: Blockchain(), for the RPC endpoint
// the indexer already polls (dialed in main.go, used in curve.go), and
// CreateLogSource, to make
// EVMI index a Curve pool the plugin just discovered.
//
// Everything here degrades quietly when Host is nil (an EVMI server older than
// the host API): the plugin keeps materializing the events it is given, and pools
// simply have to be declared by hand in autoload.config.json as before.

// defaultCurvePoolAbiName is the contract name new pool sources decode with. It
// matches the ABI declared in autoload.config.json, so a hand-declared and a
// plugin-created pool source share one ABI row.
const defaultCurvePoolAbiName = "CurveStableSwapNG"

// warnf logs to stderr (NEVER stdout — that carries the go-plugin handshake).
// Handlers receive their event in a parameter named `log`, which shadows the
// standard library package, so they warn through this instead.
func warnf(format string, args ...any) { log.Printf(format, args...) }

// registerCurvePoolSource asks EVMI to index a pool the plugin resolved, as a
// child source of the factory source that announced it. Without this the pool's
// own logs would never be delivered: the deployment event names no address, so no
// FACTORY rule can spawn the child by itself.
//
// CreateLogSource is idempotent per (parent, address), which is what makes it
// safe under at-least-once delivery — a redelivered deployment log returns the
// existing source instead of creating a second one. The event's block is the
// start block, so the pool is indexed from its creation and no earlier.
func (e *clearExporter) registerCurvePoolSource(ev exporter.LogEvent, pool string) error {
	if e.host == nil || !e.indexCurvePools {
		return nil
	}

	abiID, err := e.curvePoolAbiID()
	if err != nil {
		return fmt.Errorf("resolve abi %q: %w", e.curvePoolAbi, err)
	}

	ref, err := e.host.CreateLogSource(exporter.NewLogSource{
		Parent:     uint64(ev.SourceId),
		Address:    pool,
		Type:       exporter.SourceContract,
		AbiId:      abiID,
		StartBlock: ev.BlockNumber,
	})
	if err != nil {
		return fmt.Errorf("create log source for curve pool %s: %w", pool, err)
	}
	if ref.Created {
		log.Printf("[%s] indexing curve pool %s from block %d (source %d, abi %d)",
			e.name, pool, ev.BlockNumber, ref.Id, abiID)
	}
	return nil
}

// curvePoolAbiID resolves — once, then cached — the id of the ABI new pool
// sources decode with. An ABI already registered under that contract name is
// reused as it stands (UpsertAbi never overwrites one either, since that would
// change how every source already using it decodes); otherwise the pool ABI
// embedded below is registered, so discovering a pool does not depend on the ABI
// having been declared in autoload.config.json first.
func (e *clearExporter) curvePoolAbiID() (uint64, error) {
	e.abiMu.Lock()
	defer e.abiMu.Unlock()

	if e.curveAbiID != 0 {
		return e.curveAbiID, nil
	}
	if existing, ok, err := e.host.GetAbi(e.curvePoolAbi); err != nil {
		return 0, err
	} else if ok {
		e.curveAbiID = existing.Id
		return existing.Id, nil
	}

	ref, err := e.host.UpsertAbi(e.curvePoolAbi, curveStableSwapNGAbi)
	if err != nil {
		return 0, err
	}
	if ref.Created {
		log.Printf("[%s] registered abi %q (id %d) for discovered curve pools", e.name, e.curvePoolAbi, ref.Id)
	}
	e.curveAbiID = ref.Id
	return ref.Id, nil
}

// curveStableSwapNGAbi is the event surface of a StableSwap-NG pool — the same
// ABI autoload.config.json declares, kept here so a plugin-discovered pool can be
// indexed on a server where it was never declared. It is event-only on purpose:
// EVMI decodes logs, and an event missing from here is silently never decoded, so
// regenerate it from the contracts repo's deployed artifacts when the pool's logs
// change rather than hand-editing it.
const curveStableSwapNGAbi = `[
{"type":"event","name":"Transfer","anonymous":false,"inputs":[{"name":"sender","type":"address","indexed":true},{"name":"receiver","type":"address","indexed":true},{"name":"value","type":"uint256","indexed":false}]},
{"type":"event","name":"TokenExchange","anonymous":false,"inputs":[{"name":"buyer","type":"address","indexed":true},{"name":"sold_id","type":"int128","indexed":false},{"name":"tokens_sold","type":"uint256","indexed":false},{"name":"bought_id","type":"int128","indexed":false},{"name":"tokens_bought","type":"uint256","indexed":false}]},
{"type":"event","name":"TokenExchangeUnderlying","anonymous":false,"inputs":[{"name":"buyer","type":"address","indexed":true},{"name":"sold_id","type":"int128","indexed":false},{"name":"tokens_sold","type":"uint256","indexed":false},{"name":"bought_id","type":"int128","indexed":false},{"name":"tokens_bought","type":"uint256","indexed":false}]},
{"type":"event","name":"AddLiquidity","anonymous":false,"inputs":[{"name":"provider","type":"address","indexed":true},{"name":"token_amounts","type":"uint256[]","indexed":false},{"name":"fees","type":"uint256[]","indexed":false},{"name":"invariant","type":"uint256","indexed":false},{"name":"token_supply","type":"uint256","indexed":false}]},
{"type":"event","name":"RemoveLiquidity","anonymous":false,"inputs":[{"name":"provider","type":"address","indexed":true},{"name":"token_amounts","type":"uint256[]","indexed":false},{"name":"fees","type":"uint256[]","indexed":false},{"name":"token_supply","type":"uint256","indexed":false}]},
{"type":"event","name":"RemoveLiquidityOne","anonymous":false,"inputs":[{"name":"provider","type":"address","indexed":true},{"name":"token_id","type":"int128","indexed":false},{"name":"token_amount","type":"uint256","indexed":false},{"name":"coin_amount","type":"uint256","indexed":false},{"name":"token_supply","type":"uint256","indexed":false}]},
{"type":"event","name":"RemoveLiquidityImbalance","anonymous":false,"inputs":[{"name":"provider","type":"address","indexed":true},{"name":"token_amounts","type":"uint256[]","indexed":false},{"name":"fees","type":"uint256[]","indexed":false},{"name":"invariant","type":"uint256","indexed":false},{"name":"token_supply","type":"uint256","indexed":false}]}
]`
