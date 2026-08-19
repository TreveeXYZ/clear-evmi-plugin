package main

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]contractKind{
		"ClearBaseReserve":  baseReserveKind,
		"ClearMetaReserve":  metaReserveKind,
		"clearmetareserve":  metaReserveKind,
		"SomeReserve":       baseReserveKind,
		"ClearIOU":          iouKind,
		"CurveStableSwapNG": curveKind,
		"Pool":              curveKind,
		"ClearOracle":       oracleKind,
		"PythOracleAdapter": oracleKind,
		"ERC20":             unknownKind,
	}
	for name, want := range cases {
		if got := classify(name); got != want {
			t.Errorf("classify(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestReserveType(t *testing.T) {
	if baseReserveKind.reserveType() != "base" || metaReserveKind.reserveType() != "meta" || unknownKind.reserveType() != "reserve" {
		t.Error("reserveType mapping wrong")
	}
}

func TestNeg(t *testing.T) {
	if neg("100") != "-100" || neg("-100") != "100" || neg("0") != "0" || neg("") != "0" {
		t.Errorf("neg: %q %q %q %q", neg("100"), neg("-100"), neg("0"), neg(""))
	}
}

func TestIsZeroAddr(t *testing.T) {
	if !isZeroAddr("0x0000000000000000000000000000000000000000") {
		t.Error("zero address not detected")
	}
	if isZeroAddr("0x1111111254EEB25477B68fb85Ed929f73A960582") {
		t.Error("non-zero flagged as zero")
	}
}

func TestFirstArgAndNum(t *testing.T) {
	args := map[string]string{"sender": "0xAbC", "value": "5"}
	if firstArg(args, "from", "sender") != "0xAbC" {
		t.Error("firstArg should fall through to the Vyper name")
	}
	if firstArg(args, "from", "to") != "" {
		t.Error("firstArg should be empty when no key present")
	}
	if num(args, "value") != "5" || num(args, "missing") != "0" {
		t.Error("num default wrong")
	}
}

func TestNormAddr(t *testing.T) {
	if normAddr("0xAbCdEf") != "0xabcdef" {
		t.Error("normAddr should lowercase")
	}
}

func TestClassifyDiscoveryContracts(t *testing.T) {
	// The factory and the pool deployer both contain substrings ("reserve",
	// "pool"/"curve") that the generic reserve/curve cases would otherwise claim,
	// so they must be matched first.
	cases := map[string]contractKind{
		"ClearReserveFactory":    factoryKind,
		"clearreservefactory":    factoryKind,
		"ClearCurvePoolDeployer": curveDeployerKind,
		// The Curve factory is not a Clear reserve factory: it stays a curve contract.
		"CurveStableSwapFactoryNG": curveKind,
	}
	for name, want := range cases {
		if got := classify(name); got != want {
			t.Errorf("classify(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestKindRoundTrip(t *testing.T) {
	for _, k := range []contractKind{baseReserveKind, metaReserveKind, iouKind, curveKind, oracleKind, factoryKind, curveDeployerKind} {
		if got := kindFromString(k.String()); got != k {
			t.Errorf("kindFromString(%q) = %d, want %d", k.String(), got, k)
		}
	}
}

func TestReserveKindFromType(t *testing.T) {
	// NewClearReserve's `reserveType` is a uint8 enum (0 = base, 1 = meta); evmi may
	// hand it back numerically or symbolically.
	cases := map[string]contractKind{
		"0": baseReserveKind, "BASE_RESERVE": baseReserveKind,
		"1": metaReserveKind, "META_RESERVE": metaReserveKind,
		"2": unknownKind, "": unknownKind,
	}
	for in, want := range cases {
		if got := reserveKindFromType(in); got != want {
			t.Errorf("reserveKindFromType(%q) = %d, want %d", in, got, want)
		}
	}
}
