package property

import (
	"math"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// These are the explicit adversarial seeds from the testing guide §6. Each
// builds a hand-crafted scenario and runs it through the differential loop, so
// the engine and reference model must agree and every invariant must hold. They
// double as a permanent, human-readable regression corpus alongside the Go
// native fuzz corpus under testdata/fuzz/.

// buildStream splits a flat command list into the deposit prelude (summed into
// net deposits) and the order stream the differential loop expects.
func buildStream(cmds ...types.Command) Stream {
	s := Stream{NetDeposits: map[types.AssetID]int64{}}
	for _, c := range cmds {
		if c.Type == types.CmdDeposit {
			s.Deposits = append(s.Deposits, c)
			s.NetDeposits[c.Asset] += c.Amount
		} else {
			s.Orders = append(s.Orders, c)
		}
	}
	return s
}

func dep(acct types.AccountID, asset types.AssetID, amt int64) types.Command {
	return types.Command{Type: types.CmdDeposit, Account: acct, Asset: asset, Amount: amt}
}

// no is a new-order command on market 0 (base asset 1, quote asset 2).
func no(id types.OrderID, acct types.AccountID, side types.Side, typ types.OrderType, tif types.TIF, price, qty int64) types.Command {
	return types.Command{
		Type: types.CmdNewOrder, Market: 0, OrderID: id, Account: acct,
		Side: side, OrdType: typ, Tif: tif, Price: types.Price(price), Qty: types.Qty(qty),
	}
}

func runAdversarial(t *testing.T, name string, cmds ...types.Command) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if err := RunDifferential(buildStream(cmds...)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	})
}

func TestAdversarialBalanceBoundaries(t *testing.T) {
	const q = genQuote
	// Exact-fit: buyer quote exactly covers notional+fee of a 5@100 buy
	// (notional 500 + taker fee ceil(500*2/100)=10 = 510).
	runAdversarial(t, "exact-fit-balance",
		dep(1, 1, 100), dep(2, q, 510),
		no(1, 1, types.Sell, types.Limit, types.GTC, 100, 5),
		no(2, 2, types.Buy, types.Limit, types.GTC, 100, 5),
	)
	// One unit short: buyer cannot afford the reservation; must be rejected.
	runAdversarial(t, "one-unit-short-balance",
		dep(1, 1, 100), dep(2, q, 509),
		no(1, 1, types.Sell, types.Limit, types.GTC, 100, 5),
		no(2, 2, types.Buy, types.Limit, types.GTC, 100, 5),
	)
	// Two orders that jointly exceed available: second must be rejected.
	runAdversarial(t, "joint-over-balance",
		dep(2, q, 700),
		no(1, 2, types.Buy, types.Limit, types.GTC, 100, 3), // reserves ~306
		no(2, 2, types.Buy, types.Limit, types.GTC, 100, 4), // reserves ~408 -> over
	)
}

func TestAdversarialCrossMarketDoubleSpend(t *testing.T) {
	const q = genQuote
	// Same account, buys across all three markets whose total quote need
	// exceeds the single quote balance (INV-BAL-09): later buys are rejected.
	runAdversarial(t, "cross-market-same-account",
		dep(1, q, 1000),
		no(10, 1, types.Buy, types.Limit, types.GTC, 100, 4), // market 0
		types.Command{Type: types.CmdNewOrder, Market: 1, OrderID: 11, Account: 1, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 100, Qty: 4},
		types.Command{Type: types.CmdNewOrder, Market: 2, OrderID: 12, Account: 1, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 100, Qty: 4},
	)
}

func TestAdversarialOverflowNeighborhood(t *testing.T) {
	// Prices/quantities near int64 max must be rejected for overflow, never
	// wrap. The engine and model must agree on the rejection.
	runAdversarial(t, "int64-max-price-qty",
		dep(1, 1, 1000), dep(2, genQuote, 1000),
		no(1, 1, types.Sell, types.Limit, types.GTC, math.MaxInt64, math.MaxInt64),
		no(2, 2, types.Buy, types.Limit, types.GTC, math.MaxInt64, 5),
	)
}

func TestAdversarialFOKOneShortRollback(t *testing.T) {
	// Depth 4 against a FOK buy for 5: all-or-nothing kill, no partial fill,
	// state byte-identical to before the FOK.
	runAdversarial(t, "fok-one-short",
		dep(1, 1, 100), dep(2, genQuote, 100000),
		no(1, 1, types.Sell, types.Limit, types.GTC, 100, 4),
		no(2, 2, types.Buy, types.Limit, types.FOK, 100, 5),
	)
}

func TestAdversarialPostOnlyExactTouch(t *testing.T) {
	// Post-only buy at exactly the best ask price would cross -> rejected.
	po := no(2, 2, types.Buy, types.Limit, types.GTC, 100, 5)
	po.Flags = types.FlagPostOnly
	runAdversarial(t, "post-only-exact-touch",
		dep(1, 1, 100), dep(2, genQuote, 100000),
		no(1, 1, types.Sell, types.Limit, types.GTC, 100, 4),
		po,
	)
}

func TestAdversarialIcebergReplenishPriorityLoss(t *testing.T) {
	// An iceberg shares a level with a plain order; as the iceberg replenishes
	// it re-queues behind the plain order (priority loss).
	ice := no(1, 1, types.Sell, types.Limit, types.GTC, 100, 9)
	ice.Flags = types.FlagIceberg
	ice.DisplayQty = 2
	runAdversarial(t, "iceberg-replenish-priority-loss",
		dep(1, 1, 100), dep(3, 1, 100), dep(2, genQuote, 100000),
		ice,
		no(2, 3, types.Sell, types.Limit, types.GTC, 100, 3), // plain order behind
		no(3, 2, types.Buy, types.Limit, types.GTC, 100, 8),  // sweeps across slices
	)
}

func TestAdversarialStops(t *testing.T) {
	const q = genQuote
	// Immediate trigger: a trade sets lastPrice past a buy-stop's trigger.
	runAdversarial(t, "stop-triggers-on-trade",
		dep(1, 1, 100), dep(2, q, 100000), dep(3, q, 100000),
		no(1, 1, types.Sell, types.Limit, types.GTC, 106, 5),
		stopBuy(2, 2, 105, 2),                               // pending
		no(3, 3, types.Buy, types.Limit, types.GTC, 106, 1), // trade @106 -> trigger
	)
	// One trade triggers many stops.
	runAdversarial(t, "one-trade-triggers-many-stops",
		dep(1, 1, 100), dep(2, q, 100000), dep(3, q, 100000), dep(4, q, 100000),
		no(1, 1, types.Sell, types.Limit, types.GTC, 106, 5),
		stopBuy(2, 2, 105, 1),
		stopBuy(3, 3, 105, 1),
		no(4, 4, types.Buy, types.Limit, types.GTC, 106, 1),
	)
	// Stop triggers a stop: A's activation trades and moves price into B.
	runAdversarial(t, "stop-triggers-stop",
		dep(1, 1, 100), dep(5, 1, 100), dep(2, q, 100000), dep(3, q, 100000), dep(4, q, 100000),
		no(1, 1, types.Sell, types.Limit, types.GTC, 106, 5),
		no(2, 5, types.Sell, types.Limit, types.GTC, 108, 5),
		stopBuy(3, 2, 105, 1),                               // triggers on first trade
		stopBuy(4, 3, 107, 1),                               // triggers once price reaches 108
		no(5, 4, types.Buy, types.Limit, types.GTC, 106, 1), // trade @106 -> stop3 -> market buy -> 108
	)
}

func TestAdversarialManySamePrice(t *testing.T) {
	// Several hundred resting orders at one price (FIFO stress), then a sweep.
	cmds := []types.Command{dep(99, genQuote, 100000000)}
	var id types.OrderID
	for i := 0; i < 300; i++ {
		id++
		cmds = append(cmds, dep(types.AccountID(id), 1, 10))
		cmds = append(cmds, no(id, types.AccountID(id), types.Sell, types.Limit, types.GTC, 100, 1))
	}
	cmds = append(cmds, no(9999, 99, types.Buy, types.Limit, types.GTC, 100, 250)) // sweep 250 of 300
	runAdversarial(t, "hundreds-same-price", cmds...)
}

func TestAdversarialCancelOddities(t *testing.T) {
	runAdversarial(t, "cancel-unknown-filled-cancelled",
		dep(1, 1, 100), dep(2, genQuote, 100000),
		types.Command{Type: types.CmdCancel, OrderID: 777}, // never existed
		no(1, 1, types.Sell, types.Limit, types.GTC, 100, 5),
		no(2, 2, types.Buy, types.Limit, types.GTC, 100, 5), // fills #1
		types.Command{Type: types.CmdCancel, OrderID: 1},    // already filled
		types.Command{Type: types.CmdCancel, OrderID: 2},    // fully filled aggressor
		types.Command{Type: types.CmdCancel, OrderID: 2},    // double cancel
	)
}

func TestAdversarialEmptyBook(t *testing.T) {
	runAdversarial(t, "market-and-ioc-on-empty-book",
		dep(2, genQuote, 100000),
		no(1, 2, types.Buy, types.Market, types.GTC, 0, 5),  // market buy, empty book
		no(2, 2, types.Buy, types.Limit, types.IOC, 100, 5), // IOC, empty book
	)
}

// stopBuy is a buy Stop (-> Market on trigger) on market 0.
func stopBuy(id types.OrderID, acct types.AccountID, stopPrice, qty int64) types.Command {
	c := no(id, acct, types.Buy, types.Stop, types.GTC, 0, qty)
	c.StopPrice = types.Price(stopPrice)
	return c
}
