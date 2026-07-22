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
