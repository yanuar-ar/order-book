package harness

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// fixedStream is a small deterministic command sequence (no generator) that
// crosses orders in two markets, so it exercises reserve + match + settle. Used
// to prove BuildEngine wires both topologies to behave identically.
func fixedStream() []types.Command {
	ord := func(m types.MarketID, a types.AccountID, id types.OrderID, side types.Side, px types.Price, qty types.Qty) types.Command {
		return types.Command{Type: types.CmdNewOrder, Market: m, Account: a, OrderID: id, Side: side, OrdType: types.Limit, Tif: types.GTC, Price: px, Qty: qty}
	}
	return []types.Command{
		ord(0, 1, 10, types.Sell, 10_800_010, 5), // BTC maker ask
		ord(0, 2, 11, types.Buy, 10_800_010, 3),  // BTC taker buy, partial fill
		ord(1, 3, 12, types.Sell, 400_005, 7),    // ETH maker ask
		ord(1, 4, 13, types.Buy, 400_005, 7),     // ETH taker buy, full fill
		{Type: types.CmdCancel, Market: 0, Account: 1, OrderID: 10},
	}
}

func runStream(e Engine, cmds []types.Command) {
	for _, c := range cmds {
		for !e.Submit(c) {
			e.Step()
		}
	}
	e.Drain()
}

func ledgerDigest(e Engine) string {
	bals, fees := e.Ledger().Dump()
	return fmt.Sprintf("bals=%v fees=%v", bals, fees)
}

func TestBuildEngine_SerialAndParallelAgree(t *testing.T) {
	cfg := DefaultConfig()

	serial, cleanupS, err := BuildEngine("serial", nil, cfg)
	if err != nil {
		t.Fatalf("serial build: %v", err)
	}
	defer cleanupS()

	parallel, cleanupP, err := BuildEngine("parallel", [][]types.MarketID{{0}, {1, 2}}, cfg)
	if err != nil {
		t.Fatalf("parallel build: %v", err)
	}
	defer cleanupP()

	for _, e := range []Engine{serial, parallel} {
		Fund(e, 8)
		runStream(e, fixedStream())
	}

	if got, want := ledgerDigest(parallel), ledgerDigest(serial); got != want {
		t.Fatalf("serial and parallel ledgers diverge:\n serial   = %s\n parallel = %s", want, got)
	}
	if serial.Seq() != parallel.Seq() {
		t.Fatalf("Seq diverges: serial=%d parallel=%d", serial.Seq(), parallel.Seq())
	}
}

func TestBuildEngine_UnknownTopologyErrors(t *testing.T) {
	e, cleanup, err := BuildEngine("dist", nil, DefaultConfig())
	if err == nil {
		t.Fatal("expected an error for an unknown topology")
	}
	if e != nil || cleanup != nil {
		t.Fatal("on error, engine and cleanup must be nil")
	}
}

func TestBuildEngine_SerialCleanupIsNoop(t *testing.T) {
	_, cleanup, err := BuildEngine("serial", nil, DefaultConfig())
	if err != nil {
		t.Fatalf("serial build: %v", err)
	}
	cleanup() // must not panic for the serial topology
}

func TestParseCores(t *testing.T) {
	tests := []struct {
		in   string
		want [][]types.MarketID
	}{
		{"0;1,2", [][]types.MarketID{{0}, {1, 2}}},
		{"0", [][]types.MarketID{{0}}},
		{"0 ; 1 , 2", [][]types.MarketID{{0}, {1, 2}}}, // whitespace tolerated
		{"", nil},   // empty input
		{";;", nil}, // empty groups skipped
		{"0;bad;2", [][]types.MarketID{{0}, {2}}}, // unparseable token skipped, group dropped
	}
	for _, tt := range tests {
		if got := ParseCores(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseCores(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
