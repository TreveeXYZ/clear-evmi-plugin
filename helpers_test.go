package main

import (
	"encoding/json"
	"testing"
)

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
		// The Curve factory is not a Clear reserve factory, and not a pool either:
		// it is the contract whose deployment events pools are discovered from.
		"CurveStableSwapFactoryNG": curveFactoryKind,
		"CurveStableSwapNG":        curveKind,
	}
	for name, want := range cases {
		if got := classify(name); got != want {
			t.Errorf("classify(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestKindRoundTrip(t *testing.T) {
	for _, k := range []contractKind{baseReserveKind, metaReserveKind, iouKind, curveKind, oracleKind, factoryKind, curveDeployerKind, curveFactoryKind} {
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

// TestSplitArrayArg covers both renderings an evmi server may use for an array
// ABI arg: a real JSON array, and Go's fmt.Sprint form (what a server whose
// formatArgValue has no slice case emits — the one that caused
// "pq: invalid input syntax for type json" on NewClearReserve).
func TestSplitArrayArg(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty arg", "", nil},
		{"json strings", `["1","2"]`, []string{"1", "2"}},
		{"json numbers keep precision", `[115792089237316195423570985008687907853269984665640564039457584007913129639935,2]`,
			[]string{"115792089237316195423570985008687907853269984665640564039457584007913129639935", "2"}},
		{"json empty", "[]", nil},
		{"sprint numbers", "[1 2 3]", []string{"1", "2", "3"}},
		{"sprint addresses", "[0xAbC0000000000000000000000000000000000001 0xdEf0000000000000000000000000000000000002]",
			[]string{"0xAbC0000000000000000000000000000000000001", "0xdEf0000000000000000000000000000000000002"}},
		{"sprint single", "[42]", []string{"42"}},
		{"sprint empty", "[ ]", nil},
	}
	for _, c := range cases {
		got, err := splitArrayArg(c.in)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: element %d = %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}

	if _, err := splitArrayArg("not-an-array"); err == nil {
		t.Error("a non-array arg should be rejected, not silently accepted")
	}
}

// TestJSONArrayArg checks that whichever rendering arrives, what reaches the JSONB
// column is valid JSON — the actual bug: the fmt.Sprint form went in verbatim.
func TestJSONArrayArg(t *testing.T) {
	for _, in := range []string{`["1","2"]`, "[1 2]"} {
		got, err := jsonArrayArg(map[string]string{"amounts": in}, "amounts", false)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if !got.Valid || got.String != `["1","2"]` {
			t.Errorf("%q -> %q, want [\"1\",\"2\"]", in, got.String)
		}
		var check []string
		if err := json.Unmarshal([]byte(got.String), &check); err != nil {
			t.Errorf("%q produced invalid JSON %q: %v", in, got.String, err)
		}
	}

	// address[] is lowercased to match every other stored address.
	got, err := jsonArrayArg(map[string]string{"tokens": "[0xAbC0000000000000000000000000000000000001]"}, "tokens", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.String != `["0xabc0000000000000000000000000000000000001"]` {
		t.Errorf("tokens -> %q, want lowercased", got.String)
	}

	// Absent arg stays NULL rather than becoming an empty array.
	if got, err := jsonArrayArg(map[string]string{}, "fees", false); err != nil || got.Valid {
		t.Errorf("absent arg -> %+v, %v; want NULL", got, err)
	}
}
