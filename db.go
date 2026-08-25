package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// zeroAddress is the ERC20 mint/burn counterparty.
const zeroAddress = "0x0000000000000000000000000000000000000000"

// contractKind classifies a source contract from its ABI ContractName so the same
// event name (e.g. Transfer) can be routed to the right table. Matching is
// case-insensitive and substring-based, so ABIs only need sensible names
// (…Base…Reserve, …Meta…Reserve, …IOU…, …Curve…/…StableSwap…/…Pool…,
// …Reserve…Factory…, …Deployer…).
type contractKind int

const (
	unknownKind contractKind = iota
	baseReserveKind
	metaReserveKind
	iouKind
	curveKind
	oracleKind
	// factoryKind is the ClearReserveFactory: it announces every reserve it
	// deploys (NewClearReserve) and holds the protocol-wide config (treasury,
	// implementations).
	factoryKind
	// curveDeployerKind is the ClearCurvePoolDeployer: it announces every Curve
	// pool it deploys for a reserve (PoolDeployed).
	curveDeployerKind
	// curveFactoryKind is the CurveStableSwapFactoryNG: it announces every pool
	// deployed through it (PlainPoolDeployed / MetaPoolDeployed) — including
	// pools the ClearCurvePoolDeployer did not build — but without naming the
	// address, which is why that path needs RPC (see curve.go).
	curveFactoryKind
)

// classify is the first-sight fallback used when an address is not yet in the
// registry. Order matters: the deployer/factory names also contain "pool" and
// "reserve", so they have to be matched before the generic curve/reserve cases.
func classify(contractName string) contractKind {
	n := strings.ToLower(contractName)
	switch {
	case strings.Contains(n, "oracle"):
		return oracleKind
	case strings.Contains(n, "factory") && (strings.Contains(n, "curve") || strings.Contains(n, "stableswap")):
		return curveFactoryKind
	case strings.Contains(n, "deployer"):
		return curveDeployerKind
	case strings.Contains(n, "factory") && strings.Contains(n, "reserve"):
		return factoryKind
	case strings.Contains(n, "meta") && strings.Contains(n, "reserve"):
		return metaReserveKind
	case strings.Contains(n, "base") && strings.Contains(n, "reserve"):
		return baseReserveKind
	case strings.Contains(n, "reserve"):
		return baseReserveKind
	case strings.Contains(n, "iou"):
		return iouKind
	case strings.Contains(n, "curve"), strings.Contains(n, "stableswap"), strings.Contains(n, "pool"):
		return curveKind
	}
	return unknownKind
}

func (k contractKind) reserveType() string {
	switch k {
	case metaReserveKind:
		return "meta"
	case baseReserveKind:
		return "base"
	default:
		return "reserve"
	}
}

// normAddr lowercases an address for use as a stable key (evmi emits checksummed
// mixed-case hex).
func normAddr(a string) string {
	return strings.ToLower(strings.TrimSpace(a))
}

func isZeroAddr(a string) bool {
	return normAddr(a) == zeroAddress
}

// firstArg returns the first present, non-empty arg among keys (Solidity and
// Vyper name the same ERC20 fields differently: from/to vs sender/receiver).
func firstArg(args map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// num returns a decimal-string arg suitable for a NUMERIC column, defaulting to
// "0" when absent so inserts never fail on a NULL.
func num(args map[string]string, key string) string {
	if v, ok := args[key]; ok && v != "" {
		return v
	}
	return "0"
}

// neg negates a decimal string for debit deltas.
func neg(v string) string {
	if v == "" || v == "0" {
		return "0"
	}
	if strings.HasPrefix(v, "-") {
		return v[1:]
	}
	return "-" + v
}

// splitArrayArg decodes an array ABI argument (uint256[], address[]) into its
// elements. Two renderings have to be accepted, because which one arrives depends
// on the server version:
//
//   - a real JSON array — `["1","2"]` or `[1,2]` — from a server that serializes
//     array args;
//   - Go's fmt.Sprint form — `[1 2]`, `[0xAbC… 0xDeF…]` — from a server whose
//     formatArgValue has no slice case and falls through to fmt.Sprint. This is
//     NOT JSON: handing it to a JSONB column fails with "invalid input syntax for
//     type json", and json.Unmarshal on it fails outright.
//
// Numbers keep their literal text rather than going through float64, so a uint256
// survives intact.
func splitArrayArg(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("not an array arg: %q", s)
	}

	// JSON first — `["1","2"]` must not be split on whitespace.
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err == nil {
		out := make([]string, 0, len(raw))
		for _, el := range raw {
			var str string
			if err := json.Unmarshal(el, &str); err == nil {
				out = append(out, str)
				continue
			}
			// A number or bool: keep the literal token (uint256-safe).
			out = append(out, string(el))
		}
		return out, nil
	}

	// fmt.Sprint form: unquoted elements separated by whitespace.
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, nil
	}
	return strings.Fields(inner), nil
}

// jsonArrayArg wraps an array ABI arg for a JSONB column, NULL when absent. It
// normalizes whichever rendering the server used into a JSON array of strings —
// strings so a uint256 keeps full precision through the column. lower lowercases
// each element (for address[], to match every other stored address).
func jsonArrayArg(args map[string]string, key string, lower bool) (sql.NullString, error) {
	els, err := splitArrayArg(args[key])
	if err != nil {
		return sql.NullString{}, fmt.Errorf("%s: %w", key, err)
	}
	if els == nil {
		return sql.NullString{}, nil
	}
	if lower {
		for i := range els {
			els[i] = strings.ToLower(els[i])
		}
	}
	encoded, err := json.Marshal(els)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("%s: %w", key, err)
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}

// jsonStrings renders a list the plugin built itself (rather than one read out of
// an ABI arg) for a JSONB column, NULL when empty.
func jsonStrings(values []string) (sql.NullString, error) {
	if len(values) == 0 {
		return sql.NullString{}, nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}

// adjustBalance upserts a per-holder balance by delta (a signed decimal string).
// keyCol is the owning-contract column (reserve / token / pool). chainID scopes the
// row so the same address on two chains stays distinct.
func adjustBalance(tx *sql.Tx, chainID uint64, table, keyCol, owner, holder, delta string, block uint64) error {
	q := fmt.Sprintf(`INSERT INTO %s (chain_id, %s, holder, balance, last_block)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (chain_id, %s, holder) DO UPDATE
SET balance = %s.balance + EXCLUDED.balance, last_block = EXCLUDED.last_block`,
		table, keyCol, keyCol, table)
	_, err := tx.Exec(q, chainID, owner, holder, delta, block)
	return err
}

// adjustTokenBalance upserts a base reserve's physical holding of one underlying
// asset by a signed decimal delta. Additive like adjustBalance (so it stays exact
// under the exactly-once ledger); a zero/empty delta or missing asset is a no-op.
func adjustTokenBalance(tx *sql.Tx, chainID uint64, reserve, asset, delta string, block uint64) error {
	if asset == "" || delta == "" || delta == "0" || delta == "-0" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO clear_reserve_token_balances (chain_id, reserve, asset, balance, last_block)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (chain_id, reserve, asset) DO UPDATE
SET balance = clear_reserve_token_balances.balance + EXCLUDED.balance, last_block = EXCLUDED.last_block`,
		chainID, reserve, asset, delta, block)
	return err
}
