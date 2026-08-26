package main

import (
	"database/sql"
	"errors"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/lmittmann/w3"
	"github.com/lmittmann/w3/module/eth"
	"github.com/lmittmann/w3/w3types"
)

// Resolving a Curve StableSwap-NG deployment — the plugin's only chain access.
//
// PlainPoolDeployed(coins[], A, fee, deployer) and MetaPoolDeployed(coin,
// base_pool, A, fee, deployer) are all the factory logs, and NEITHER carries the
// address of the pool it just created (contracts/vyper/CurveStableSwapFactoryNG.vy
// — deploy_plain_pool/deploy_metapool return the address, they do not log it).
// That is precisely the gap Host.CreateLogSource exists for: no FACTORY rule can
// spawn a child from an event with no address in it, so the plugin has to resolve
// the address itself and register the source.
//
// It is resolved from the deployment transaction's own receipt rather than from
// factory storage: every StableSwap-NG pool fires `Transfer(0x0, msg.sender, 0)`
// at the end of its constructor (CurveStableSwapNG.vy / CurveStableSwapMetaNG.vy),
// and msg.sender there is the factory, since the pool is created with
// create_from_blueprint. So the pool is the emitter of the last such log before
// the deploy event in the same transaction.
//
// That beats reading pool_list/pool_count for two reasons: it needs no archive
// state (the index the pool got is historical, the receipt is not), and it stays
// exact when one transaction deploys several pools — which Clear's
// ClearCurvePoolDeployer does — because each deploy event is paired with the
// constructor log immediately preceding it.
//
// This file is the only one that knows about go-ethereum / w3 types; everything
// it hands back is the plain lowercase-hex-and-decimal-string world the rest of
// the plugin (and the schema) works in. It also carries the one other chain read
// the plugin makes — an ERC20's name/symbol/decimals on first sight of a token
// (fetchTokenMeta), which reuses the same three bindings a pool is read with.

// ABI bindings, from Solidity/Vyper signatures rather than a generated binding —
// they are usable against any contract exposing that function.
var (
	// eventTransfer's constructor mint is how a freshly deployed pool announces
	// itself: Transfer(0x0, factory, 0), emitted before the deployment event.
	eventTransfer = w3.MustNewEvent("Transfer(address indexed from, address indexed to, uint256 value)")

	// Read off the pool itself.
	funcName     = w3.MustNewFunc("name()", "string")
	funcSymbol   = w3.MustNewFunc("symbol()", "string")
	funcDecimals = w3.MustNewFunc("decimals()", "uint8")

	// Read off the factory, which is where a deployed pool's registry entry lives.
	funcGetCoins          = w3.MustNewFunc("get_coins(address)", "address[]")
	funcGetDecimals       = w3.MustNewFunc("get_decimals(address)", "uint256[]")
	funcGetImplementation = w3.MustNewFunc("get_implementation_address(address)", "address")
	funcIsMeta            = w3.MustNewFunc("is_meta(address)", "bool")
)

// resolveDeployedPool returns the address of the pool created by the deploy event
// at deployLogIndex in transaction txHash. It returns "" (no error) when the
// receipt holds no constructor log that could be one — retrying would not help
// there, so the caller warns and skips rather than stalling the pipeline. A
// transport error IS returned, because that one is worth retrying.
func resolveDeployedPool(client *w3.Client, factoryHex, txHex string, deployLogIndex uint64) (string, error) {
	txHash := common.HexToHash(txHex)
	if txHash == (common.Hash{}) {
		warnf("[clear-defi] malformed transaction hash %q, cannot resolve the deployed pool", txHex)
		return "", nil
	}
	factory := common.HexToAddress(factoryHex)

	var receipt *types.Receipt
	if err := client.Call(eth.TxReceipt(txHash).Returns(&receipt)); err != nil {
		return "", err
	}

	var (
		pool      common.Address
		poolIndex uint
	)
	for _, l := range receipt.Logs {
		if uint64(l.Index) >= deployLogIndex || l.Address == factory {
			continue
		}
		var (
			from, to common.Address
			value    *big.Int
		)
		if err := eventTransfer.DecodeArgs(l, &from, &to, &value); err != nil {
			continue // not an ERC20 Transfer
		}
		// A mint of nothing, to the factory, from a contract that is not the
		// factory: the shape of a StableSwap-NG constructor's Transfer.
		if from != w3.Addr0 || to != factory || value == nil || value.Sign() != 0 {
			continue
		}
		if pool == w3.Addr0 || l.Index > poolIndex {
			pool, poolIndex = l.Address, l.Index
		}
	}
	if pool == w3.Addr0 {
		return "", nil
	}
	return normAddr(pool.Hex()), nil
}

// curvePoolInfo is what the chain still knows about a freshly deployed pool that
// its deployment event does not carry. Every field is best-effort: a getter that
// reverts or is absent leaves its column NULL rather than failing the event.
type curvePoolInfo struct {
	name           string
	symbol         string
	decimals       sql.NullInt64
	implementation string
	isMeta         sql.NullBool
	coins          []string
	coinDecimals   []string
}

// fetchCurvePool reads a new pool's metadata back in ONE batched request —
// name/symbol/decimals from the pool itself, and the registry view (coins, their
// decimals, the blueprint it was cloned from, whether it is a metapool) from the
// factory, which is the contract that actually stores it. Batching matters
// because this endpoint is the one the indexer polls: seven getters cost one
// round trip, not seven.
//
// w3 reports a per-call failure through w3.CallErrors while still filling in
// every call that DID succeed, so a single getter that reverts (an older factory,
// a pool its registry does not know) leaves only its own column NULL. A failure
// of the request itself yields no metadata at all — still not fatal, since the
// pool row and its log source matter more than the decoration.
func fetchCurvePool(client *w3.Client, factoryHex, poolHex string) curvePoolInfo {
	factory, pool := common.HexToAddress(factoryHex), common.HexToAddress(poolHex)

	var (
		name        string
		symbol      string
		decimals    uint8
		coins       []common.Address
		regDecimals []*big.Int
		implAddr    common.Address
		isMeta      bool
	)
	labels := []string{
		"name()", "symbol()", "decimals()",
		"get_coins", "get_decimals", "get_implementation_address", "is_meta",
	}
	calls := []w3types.RPCCaller{
		eth.CallFunc(pool, funcName).Returns(&name),
		eth.CallFunc(pool, funcSymbol).Returns(&symbol),
		eth.CallFunc(pool, funcDecimals).Returns(&decimals),
		eth.CallFunc(factory, funcGetCoins, pool).Returns(&coins),
		eth.CallFunc(factory, funcGetDecimals, pool).Returns(&regDecimals),
		eth.CallFunc(factory, funcGetImplementation, pool).Returns(&implAddr),
		eth.CallFunc(factory, funcIsMeta, pool).Returns(&isMeta),
	}

	var callErrs w3.CallErrors
	if err := client.Call(calls...); errors.As(err, &callErrs) {
		for i, callErr := range callErrs {
			if callErr != nil {
				warnf("[clear-defi] curve pool %s: %s: %v", poolHex, labels[i], callErr)
			}
		}
	} else if err != nil {
		warnf("[clear-defi] curve pool %s: metadata request failed: %v", poolHex, err)
		return curvePoolInfo{}
	}
	// A call whose slot in callErrs is nil answered; anything else is left unset,
	// so a reverted getter is NULL rather than a zero value pretending to be data.
	ok := func(i int) bool { return i >= len(callErrs) || callErrs[i] == nil }

	var info curvePoolInfo
	if ok(0) {
		info.name = strings.TrimSpace(name)
	}
	if ok(1) {
		info.symbol = strings.TrimSpace(symbol)
	}
	if ok(2) {
		info.decimals = sql.NullInt64{Int64: int64(decimals), Valid: true}
	}
	if ok(3) {
		info.coins = make([]string, len(coins))
		for i, coin := range coins {
			info.coins[i] = normAddr(coin.Hex())
		}
	}
	if ok(4) {
		// Kept positional: entry i belongs to coin i, so a missing one must hold
		// its slot rather than shift the rest.
		info.coinDecimals = make([]string, len(regDecimals))
		for i, dec := range regDecimals {
			if dec != nil {
				info.coinDecimals[i] = dec.String()
			}
		}
	}
	if ok(5) && implAddr != w3.Addr0 {
		info.implementation = normAddr(implAddr.Hex())
	}
	if ok(6) {
		info.isMeta = sql.NullBool{Bool: isMeta, Valid: true}
	}
	return info
}

// --- ERC20 metadata (any token, not just Curve) ---

// tokenMeta is what an ERC20 says about itself. It doubles as the hint type for
// ensureToken: a discovery event that already carries one of these fields (a
// reserve's `decimals` on AssetAdded, a reserve's name/symbol on NewClearReserve)
// passes it in, and a complete set of hints skips the RPC round trip entirely.
type tokenMeta struct {
	name     string
	symbol   string
	decimals sql.NullInt64
}

// complete reports whether every field is known, i.e. nothing is left to fetch.
func (m tokenMeta) complete() bool {
	return m.name != "" && m.symbol != "" && m.decimals.Valid
}

// empty reports whether nothing at all is known, i.e. there is nothing to write.
func (m tokenMeta) empty() bool { return m == tokenMeta{} }

// withFallback fills in whatever the getters did not answer from the values the
// discovery event carried.
func (m tokenMeta) withFallback(hints tokenMeta) tokenMeta {
	if m.name == "" {
		m.name = hints.name
	}
	if m.symbol == "" {
		m.symbol = hints.symbol
	}
	if !m.decimals.Valid {
		m.decimals = hints.decimals
	}
	return m
}

// fetchTokenMeta reads an ERC20's name/symbol/decimals in one batched request.
// Individual getters are best-effort — a token that does not implement one, or
// that returns bytes32 for name/symbol the way some early ERC20s do, leaves that
// field unset rather than failing the token. Only a failure of the request itself
// is returned, because that one is worth retrying.
func fetchTokenMeta(client *w3.Client, addrHex string) (tokenMeta, error) {
	token := common.HexToAddress(addrHex)

	var (
		name     string
		symbol   string
		decimals uint8
	)
	labels := []string{"name()", "symbol()", "decimals()"}
	calls := []w3types.RPCCaller{
		eth.CallFunc(token, funcName).Returns(&name),
		eth.CallFunc(token, funcSymbol).Returns(&symbol),
		eth.CallFunc(token, funcDecimals).Returns(&decimals),
	}

	var callErrs w3.CallErrors
	if err := client.Call(calls...); errors.As(err, &callErrs) {
		for i, callErr := range callErrs {
			if callErr != nil {
				warnf("[clear-defi] token %s: %s: %v", addrHex, labels[i], callErr)
			}
		}
	} else if err != nil {
		return tokenMeta{}, err
	}
	ok := func(i int) bool { return i >= len(callErrs) || callErrs[i] == nil }

	var meta tokenMeta
	if ok(0) {
		meta.name = strings.TrimSpace(name)
	}
	if ok(1) {
		meta.symbol = strings.TrimSpace(symbol)
	}
	if ok(2) {
		meta.decimals = sql.NullInt64{Int64: int64(decimals), Valid: true}
	}
	return meta, nil
}

// eventCoins is the coin list taken from the deployment event itself, used when
// the factory getter could not be read: PlainPoolDeployed carries the whole
// `coins` array, and a metapool is always [coin, base pool LP] — for a
// StableSwap-NG base pool the LP token IS the pool, so that is `base_pool`.
func eventCoins(args map[string]string, meta bool) ([]string, error) {
	if meta {
		var out []string
		for _, key := range []string{"coin", "base_pool"} {
			if addr := normAddr(args[key]); addr != "" && !isZeroAddr(addr) {
				out = append(out, addr)
			}
		}
		return out, nil
	}
	coins, err := splitArrayArg(args["coins"])
	if err != nil {
		return nil, err
	}
	for i := range coins {
		coins[i] = normAddr(coins[i])
	}
	return coins, nil
}

// coinDecimals returns the i-th entry of a factory get_decimals result, NULL when
// the call failed or the list is shorter than the coin list.
func coinDecimals(decs []string, i int) sql.NullInt64 {
	if i >= len(decs) {
		return sql.NullInt64{}
	}
	v, err := strconv.ParseInt(decs[i], 10, 64)
	if err != nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}
