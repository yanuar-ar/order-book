// Package market assembles the engine: it wraps each market's book + matcher as
// a shard and wires the sequencer, balance authority, and shards together.
//
// v1 runs single-threaded: the sequencer's routing thread matches and settles
// inline. Because commands are processed in Seq order and a command's fills all
// carry that Seq as AggressorSeq, inline settlement equals the deterministic
// (aggressor_seq, match_index) order. The concurrent shard-goroutine topology
// with fill rings is deferred to the performance phase.
package market

import (
	"github.com/yanuar-ar/order-book/internal/matching"
	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

// Shard owns one market's book and matching engine.
type Shard struct {
	Market types.MarketID
	engine *matching.Engine
}

// NewShard builds a shard for market with an arena capacity hint and the
// quantity scale (for market-buy funds capping). The stop activation sink is
// installed later via SetSink (engine assembly wires it to the sequencer's
// re-injection entry).
func NewShard(market types.MarketID, capHint int, qtyScale int64) *Shard {
	return &Shard{Market: market, engine: matching.NewEngine(orderbook.New(market, capHint), nil, qtyScale)}
}

// SetSink installs the stop-activation sink.
func (s *Shard) SetSink(sink matching.Sink) { s.engine.SetSink(sink) }

// Submit matches a funded order (or stores a stop) and returns the result.
func (s *Shard) Submit(o types.FundedOrder) matching.Result { return s.engine.Submit(o) }

// Cancel removes a resting order or pending stop.
func (s *Shard) Cancel(id types.OrderID) bool { return s.engine.Cancel(id) }

// AmendDown reduces a resting order's quantity in place, keeping priority.
func (s *Shard) AmendDown(id types.OrderID, newQty types.Qty) bool {
	return s.engine.Book().AmendDown(id, newQty)
}

// Book exposes the shard's book (read access for invariants/tests).
func (s *Shard) Book() *orderbook.Book { return s.engine.Book() }

// StopDump exposes the shard's pending stop orders (read access for
// invariants/tests).
func (s *Shard) StopDump() []matching.StopView { return s.engine.StopDump() }
