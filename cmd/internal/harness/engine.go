// Package harness holds the shared building blocks for the engine benchmark
// tools (cmd/throughput, cmd/loadtest): the topology-keyed engine builder and
// its interface, the load generators, the latency histogram, and the live
// order-book TUI. The tools are thin drivers over this kit; topology
// (serial vs parallel) is a builder input, not a tool boundary.
package harness

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
)

// Asset and market ids shared by every harness tool: markets 0/1/2 are
// BTC/ETH/SOL quoted in USDT.
const (
	USDT types.AssetID = 2
	BTC  types.AssetID = 1
	ETH  types.AssetID = 3
	SOL  types.AssetID = 4

	// QtyDiv scales display: quantities are 1e-8 base units (satoshis).
	QtyDiv = 100_000_000
)

// MarketBase maps each market id to its base asset (quote is always USDT).
var MarketBase = map[types.MarketID]types.AssetID{0: BTC, 1: ETH, 2: SOL}

// MarketName maps each market id to its display symbol.
var MarketName = map[types.MarketID]string{0: "BTC/USDT", 1: "ETH/USDT", 2: "SOL/USDT"}

// MarketMid is each market's real-world starting mid in cents (1 tick = $0.01):
// BTC ~ $108,000, ETH ~ $4,000, SOL ~ $200.
var MarketMid = map[types.MarketID]types.Price{0: 10_800_000, 1: 400_000, 2: 20_000}

// Engine is the read/drive surface the benchmark tools need; both
// *market.Engine and *market.ParallelEngine satisfy it without modification.
type Engine interface {
	Submit(types.Command) bool
	Step() bool
	Drain()
	Acks() []types.Ack
	Seq() types.Seq
	Shard(types.MarketID) *market.Shard
	Ledger() *balance.Ledger
	MarketIDs() []types.MarketID
}

// DefaultConfig returns the standard three-market engine config the tools use
// (1% maker / 2% taker fee at scale 100, large rings and arena).
func DefaultConfig() market.Config {
	specs := map[types.MarketID]balance.MarketSpec{}
	for m, base := range MarketBase {
		specs[m] = balance.MarketSpec{Base: base, Quote: USDT}
	}
	return market.Config{
		Markets: specs, QtyScale: QtyDiv, FeeScale: 100, MakerFee: 1, TakerFee: 2,
		RingSize: 1 << 16, CapHint: 1 << 20,
	}
}

// BuildEngine constructs an engine for the named topology and returns it with a
// cleanup func. "serial" builds a *market.Engine (cleanup is a no-op); "parallel"
// builds a *market.ParallelEngine pinned per the groups assignment (cleanup
// stops its worker goroutines). A nil/empty groups gives each market its own
// worker. An unknown topology is an error, never a panic.
func BuildEngine(topology string, groups [][]types.MarketID, cfg market.Config) (Engine, func(), error) {
	switch topology {
	case "serial":
		e := market.NewEngine(cfg)
		return e, func() { _ = e.Close() }, nil
	case "parallel":
		pe := market.NewParallelEngine(cfg, groups)
		return pe, pe.Close, nil
	default:
		return nil, nil, fmt.Errorf("unknown topology %q (want \"serial\" or \"parallel\")", topology)
	}
}

// ParseCores parses a market→worker map: ';' separates workers, ',' separates
// the markets on a worker. E.g. "0;1,2" → [[0],[1,2]] (market 0 alone on worker
// 0; markets 1 and 2 sharing worker 1). Empty groups and unparseable tokens are
// skipped. Used only by the parallel topology.
func ParseCores(s string) [][]types.MarketID {
	var groups [][]types.MarketID
	for _, grp := range strings.Split(s, ";") {
		grp = strings.TrimSpace(grp)
		if grp == "" {
			continue
		}
		var markets []types.MarketID
		for _, tok := range strings.Split(grp, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(tok)); err == nil {
				markets = append(markets, types.MarketID(n))
			}
		}
		if len(markets) > 0 {
			groups = append(groups, markets)
		}
	}
	return groups
}

// Fund deposits a generous balance for each of users accounts: USDT plus every
// base asset, so order generation never trips an insufficient-funds rejection
// for balance reasons.
func Fund(e Engine, users int) {
	for a := 1; a <= users; a++ {
		e.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: USDT, Amount: 1 << 54})
		for _, base := range MarketBase {
			e.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: base, Amount: 1 << 50})
		}
	}
	e.Drain()
}
