package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/lmittmann/w3"
)

// The Transfer binding is what a deployed pool is recognised by, so its topic is
// checked against the value fixed by the ERC20 standard itself.
func TestTransferEventBinding(t *testing.T) {
	const transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	if got := eventTransfer.Topic0.Hex(); got != transferTopic {
		t.Errorf("eventTransfer.Topic0 = %s, want %s", got, transferTopic)
	}
}

// constructorTransferLog is the Transfer(0x0, factory, 0) a StableSwap-NG pool
// fires at the end of its constructor — the marker resolveDeployedPool keys on.
func constructorTransferLog(pool, factory string, idx uint) *types.Log {
	return transferLog(pool, common.Address{}.Hex(), factory, new(big.Int), idx)
}

func transferLog(emitter, from, to string, value *big.Int, idx uint) *types.Log {
	return &types.Log{
		Address: common.HexToAddress(emitter),
		Topics: []common.Hash{
			eventTransfer.Topic0,
			common.HexToHash(from),
			common.HexToHash(to),
		},
		Data:  common.BigToHash(value).Bytes(),
		Index: idx,
	}
}

// receiptNode stands in for an RPC endpoint that only answers
// eth_getTransactionReceipt, keyed by transaction hash.
func receiptNode(t *testing.T, receipts map[string][]*types.Log) *w3.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Method != "eth_getTransactionReceipt" {
			http.Error(w, "unexpected method "+req.Method, http.StatusBadRequest)
			return
		}
		var hash string
		json.Unmarshal(req.Params[0], &hash)
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": &types.Receipt{
				Status: types.ReceiptStatusSuccessful,
				TxHash: common.HexToHash(hash),
				Logs:   receipts[hash],
			},
		})
	}))
	t.Cleanup(srv.Close)

	client, err := w3.Dial(srv.URL)
	if err != nil {
		t.Fatalf("dial stub node: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// A single transaction deploying two pools is the case that matters: Clear's
// ClearCurvePoolDeployer does exactly that, so each deploy event has to be paired
// with the constructor Transfer immediately preceding it — not with whichever one
// happens to be in the receipt.
func TestResolveDeployedPool(t *testing.T) {
	const (
		factory = "0x00000000000000000000000000000000000000ff"
		pool1   = "0x1111111111111111111111111111111111111111"
		pool2   = "0x2222222222222222222222222222222222222222"
		token   = "0x3333333333333333333333333333333333333333"
		txHash  = "0x00000000000000000000000000000000000000000000000000000000000000a1"
	)

	client := receiptNode(t, map[string][]*types.Log{
		txHash: {
			// An ordinary token transfer to the factory: not a mint, must be ignored.
			transferLog(token, pool1, factory, big.NewInt(100), 0),
			constructorTransferLog(pool1, factory, 1),
			{Address: common.HexToAddress(factory), Topics: []common.Hash{{0xde, 0xad}}, Index: 2},
			constructorTransferLog(pool2, factory, 3),
			{Address: common.HexToAddress(factory), Topics: []common.Hash{{0xde, 0xad}}, Index: 4},
		},
	})

	for _, tc := range []struct {
		deployLogIndex uint64
		want           string
	}{
		{2, pool1},
		{4, pool2},
		{1, ""}, // nothing deployed before the first constructor log
	} {
		got, err := resolveDeployedPool(client, factory, txHash, tc.deployLogIndex)
		if err != nil {
			t.Fatalf("resolveDeployedPool(%d): %v", tc.deployLogIndex, err)
		}
		if got != tc.want {
			t.Errorf("resolveDeployedPool(%d) = %q, want %q", tc.deployLogIndex, got, tc.want)
		}
	}
}

// A transaction hash that is not 32 bytes of hex would silently become the zero
// hash and fetch someone else's receipt, so it has to be rejected up front.
func TestResolveDeployedPoolRejectsBadHash(t *testing.T) {
	client := receiptNode(t, nil)
	got, err := resolveDeployedPool(client, "0x00000000000000000000000000000000000000ff", "0xtx1111", 2)
	if err != nil || got != "" {
		t.Errorf("resolveDeployedPool with a malformed hash = %q, %v; want \"\", nil", got, err)
	}
}

func TestEventCoins(t *testing.T) {
	// PlainPoolDeployed carries the whole coins array — in whichever of the two
	// renderings the server used (see splitArrayArg).
	plain := map[string]string{"coins": "[0xAAaa 0xBBbb]"}
	got, err := eventCoins(plain, false)
	if err != nil {
		t.Fatalf("eventCoins plain: %v", err)
	}
	if fmt.Sprint(got) != "[0xaaaa 0xbbbb]" {
		t.Errorf("eventCoins plain = %v", got)
	}

	// A metapool is always [coin, base pool LP], and for StableSwap-NG the base
	// pool's LP token IS the base pool.
	meta := map[string]string{"coin": "0xCoIn", "base_pool": "0xBaSe"}
	got, err = eventCoins(meta, true)
	if err != nil {
		t.Fatalf("eventCoins meta: %v", err)
	}
	if fmt.Sprint(got) != "[0xcoin 0xbase]" {
		t.Errorf("eventCoins meta = %v", got)
	}
}

func TestCoinDecimals(t *testing.T) {
	decs := []string{"18", "6"}
	if d := coinDecimals(decs, 1); !d.Valid || d.Int64 != 6 {
		t.Errorf("coinDecimals(1) = %+v", d)
	}
	if d := coinDecimals(decs, 2); d.Valid {
		t.Error("coinDecimals past the end should be NULL")
	}
	// A slot the factory getter left empty must stay NULL, not become 0.
	if d := coinDecimals([]string{""}, 0); d.Valid {
		t.Error("coinDecimals of an empty slot should be NULL")
	}
}
