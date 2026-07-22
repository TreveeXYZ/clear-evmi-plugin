package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// zeroAddress is the ERC20 mint/burn counterparty.
const zeroAddress = "0x0000000000000000000000000000000000000000"

// contractKind classifies a source contract from its ABI ContractName so the same
// event name (e.g. Transfer) can be routed to the right table. Matching is
// case-insensitive and substring-based, so ABIs only need sensible names
// (…Base…Reserve, …Meta…Reserve, …IOU…, …Curve…/…StableSwap…/…Pool…).
type contractKind int

const (
	unknownKind contractKind = iota
	baseReserveKind
	metaReserveKind
	iouKind
	curveKind
)

func classify(contractName string) contractKind {
	n := strings.ToLower(contractName)
	switch {
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

// jsonArg wraps a JSON-array arg (e.g. Curve token_amounts) for a JSONB column,
// NULL when absent.
func jsonArg(args map[string]string, key string) sql.NullString {
	if v, ok := args[key]; ok && v != "" {
		return sql.NullString{String: v, Valid: true}
	}
	return sql.NullString{}
}

// adjustBalance upserts a per-holder balance by delta (a signed decimal string).
// keyCol is the owning-contract column (reserve / token / pool).
func adjustBalance(tx *sql.Tx, table, keyCol, owner, holder, delta string, block uint64) error {
	q := fmt.Sprintf(`INSERT INTO %s (%s, holder, balance, last_block)
VALUES ($1, $2, $3, $4)
ON CONFLICT (%s, holder) DO UPDATE
SET balance = %s.balance + EXCLUDED.balance, last_block = EXCLUDED.last_block`,
		table, keyCol, keyCol, table)
	_, err := tx.Exec(q, owner, holder, delta, block)
	return err
}
