package market

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/types"
)

func snapCfg(taker int64) Config {
	return Config{
		Markets:  map[types.MarketID]balance.MarketSpec{m0: {Base: btc, Quote: usdt}},
		QtyScale: 1, FeeScale: 100, MakerFee: 1, TakerFee: taker, RingSize: 1024, CapHint: 256,
	}
}

// populated builds an engine with a partially-filled iceberg, a resting bid, and
// a pending stop — exercising every snapshot section.
func populated(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(snapCfg(2))
	run(t, e,
		dep(2, btc, 100),
		dep(1, usdt, 100000),
		// Seller posts an iceberg (qty 10, visible 3).
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 2, OrderID: 20, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Flags: types.FlagIceberg, Price: 100, Qty: 10, DisplayQty: 3},
		// Buyer takes 4 → iceberg partially filled and refilled mid-chunk.
		order(m0, 1, 10, types.Buy, types.Limit, 100, 4),
		// A resting bid that does not cross.
		order(m0, 1, 11, types.Buy, types.Limit, 90, 5),
		// A pending buy-stop above the market.
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 1, OrderID: 30, Side: types.Buy, OrdType: types.Stop, Tif: types.GTC, StopPrice: 120, Qty: 2},
	)
	return e
}

func sameState(t *testing.T, a, b *Engine) {
	t.Helper()
	ab, af := a.Ledger().Dump()
	bb, bf := b.Ledger().Dump()
	if !reflect.DeepEqual(ab, bb) {
		t.Fatalf("balances differ:\n want %+v\n got  %+v", ab, bb)
	}
	if !reflect.DeepEqual(af, bf) {
		t.Fatalf("fees differ:\n want %+v\n got  %+v", af, bf)
	}
	if !reflect.DeepEqual(a.Ledger().ReservedOrders(), b.Ledger().ReservedOrders()) {
		t.Fatalf("reserved orders differ")
	}
	for _, m := range a.MarketIDs() {
		if !reflect.DeepEqual(a.Shard(m).Book().Dump(), b.Shard(m).Book().Dump()) {
			t.Fatalf("book %d differs:\n want %+v\n got  %+v", m, a.Shard(m).Book().Dump(), b.Shard(m).Book().Dump())
		}
		if !reflect.DeepEqual(a.Shard(m).StopDump(), b.Shard(m).StopDump()) {
			t.Fatalf("stops %d differ", m)
		}
	}
	if a.Seq() != b.Seq() {
		t.Fatalf("seq differs: %d vs %d", a.Seq(), b.Seq())
	}
}

// ---- Positive ----

func TestEngineSnapshot_RoundTripEquivalence(t *testing.T) {
	e := populated(t)
	path := filepath.Join(t.TempDir(), "snap")
	if err := e.Snapshot(path); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got, err := Restore(snapCfg(2), path)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	sameState(t, e, got)
	if err := got.core.ledger.Verify(); err != nil {
		t.Fatalf("restored ledger fails Verify: %v", err)
	}
}

// ---- Edge: deterministic bytes ----

func TestEngineSnapshot_DeterministicBytes(t *testing.T) {
	e := populated(t)
	dir := t.TempDir()
	p1, p2 := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := e.Snapshot(p1); err != nil {
		t.Fatalf("snapshot a: %v", err)
	}
	if err := e.Snapshot(p2); err != nil {
		t.Fatalf("snapshot b: %v", err)
	}
	b1, _ := os.ReadFile(p1)
	b2, _ := os.ReadFile(p2)
	if !reflect.DeepEqual(b1, b2) {
		t.Fatal("two snapshots of the same logical state produced different bytes")
	}
}

// ---- Edge: header mismatches reject ----

func TestEngineSnapshot_MoneyScaleMismatchRejected(t *testing.T) {
	e := populated(t)
	path := filepath.Join(t.TempDir(), "snap")
	if err := e.Snapshot(path); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Restore with a different taker fee — the integers were computed under fee=2.
	if _, err := Restore(snapCfg(5), path); err != ErrSnapshotIncompatible {
		t.Fatalf("expected ErrSnapshotIncompatible on fee mismatch, got %v", err)
	}
}

func TestEngineSnapshot_MarketLayoutMismatchRejected(t *testing.T) {
	e := populated(t)
	path := filepath.Join(t.TempDir(), "snap")
	if err := e.Snapshot(path); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	bad := snapCfg(2)
	bad.Markets = map[types.MarketID]balance.MarketSpec{m0: {Base: eth, Quote: usdt}} // base asset changed
	if _, err := Restore(bad, path); err != ErrSnapshotIncompatible {
		t.Fatalf("expected ErrSnapshotIncompatible on market-layout mismatch, got %v", err)
	}
}

// ---- Negative: corrupt file ----

func TestEngineSnapshot_CorruptFileRejected(t *testing.T) {
	e := populated(t)
	path := filepath.Join(t.TempDir(), "snap")
	if err := e.Snapshot(path); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	raw, _ := os.ReadFile(path)
	raw[len(raw)/2] ^= 0xFF // flip a byte → CRC fails
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := Restore(snapCfg(2), path); err == nil {
		t.Fatal("Restore accepted a CRC-corrupt snapshot")
	}
}
