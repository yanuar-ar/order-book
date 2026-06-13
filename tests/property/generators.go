package property

import (
	"math/rand"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/tests/refmodel"
)

// Shared market/asset layout and economics for the differential harness: three
// markets sharing a quote asset. Kept in non-test code so the generators,
// differential loop, fuzz target, and state-machine test all agree.
const (
	genQuote    types.AssetID = 2
	genAccounts               = 6
	genQtyScale               = 1
	genFeeScale               = 100
	genMakerFee               = 1
	genTakerFee               = 2
)

// genMarkets and genBases are parallel ordered slices (market i trades genBases[i]
// against genQuote). Ordered slices, never ranged maps, keep generation deterministic.
var (
	genMarkets = []types.MarketID{0, 1, 2}
	genBases   = []types.AssetID{1, 3, 4}
)

func engineCfg() market.Config {
	specs := map[types.MarketID]balance.MarketSpec{}
	for i, m := range genMarkets {
		specs[m] = balance.MarketSpec{Base: genBases[i], Quote: genQuote}
	}
	return market.Config{Markets: specs, QtyScale: genQtyScale, FeeScale: genFeeScale, MakerFee: genMakerFee, TakerFee: genTakerFee, RingSize: 1 << 14, CapHint: 4096}
}

func modelCfg() refmodel.Config {
	specs := map[types.MarketID]refmodel.MarketSpec{}
	for i, m := range genMarkets {
		specs[m] = refmodel.MarketSpec{Base: genBases[i], Quote: genQuote}
	}
	return refmodel.Config{Markets: specs, QtyScale: genQtyScale, FeeScale: genFeeScale, MakerFee: genMakerFee, TakerFee: genTakerFee}
}

// Stream is a deterministic deposit prelude plus an order stream, with the net
// deposits per asset for the conservation check.
type Stream struct {
	Deposits    []types.Command
	Orders      []types.Command
	NetDeposits map[types.AssetID]int64
}

// standardPrelude returns a generously-funded deposit prelude and its
// net-deposits map. Shared by the fuzz target and state-machine test, which
// supply their own order streams.
func standardPrelude() ([]types.Command, map[types.AssetID]int64) {
	net := map[types.AssetID]int64{}
	var deps []types.Command
	for a := types.AccountID(1); a <= genAccounts; a++ {
		deps = append(deps, types.Command{Type: types.CmdDeposit, Account: a, Asset: genQuote, Amount: 1_000_000})
		net[genQuote] += 1_000_000
		for _, b := range genBases {
			deps = append(deps, types.Command{Type: types.CmdDeposit, Account: a, Asset: b, Amount: 10_000})
			net[b] += 10_000
		}
	}
	return deps, net
}

// GenBroad builds a uniform-random stream over wide price/qty bands with
// generous balances. It exercises every order type but most orders do not
// cross. Pure function of (seed, n).
func GenBroad(seed int64, n int) Stream { return genStream(seed, n, false) }

// GenSharp builds an adversarial-biased stream: prices clustered tightly at mid
// so orders frequently cross, tight balances that stress the budget/reservation
// paths, small-display icebergs, near-trigger stops, and frequent cancel/amend
// of live orders. Pure function of (seed, n).
func GenSharp(seed int64, n int) Stream { return genStream(seed, n, true) }

func genStream(seed int64, n int, sharp bool) Stream {
	r := rand.New(rand.NewSource(seed))
	net := map[types.AssetID]int64{}

	depQuote, depBase := int64(1_000_000), int64(10_000)
	if sharp {
		depQuote, depBase = 3_000, 300 // tight balances stress reservation/budget paths
	}
	var deposits []types.Command
	for a := types.AccountID(1); a <= genAccounts; a++ {
		deposits = append(deposits, types.Command{Type: types.CmdDeposit, Account: a, Asset: genQuote, Amount: depQuote})
		net[genQuote] += depQuote
		for _, b := range genBases {
			deposits = append(deposits, types.Command{Type: types.CmdDeposit, Account: a, Asset: b, Amount: depBase})
			net[b] += depBase
		}
	}

	priceLo, priceSpan := 90, 21 // 90..110
	maxQty := 10
	if sharp {
		priceLo, priceSpan = 98, 5 // 98..102: dense, crossing-prone
		maxQty = 20
	}
	randPrice := func() types.Price { return types.Price(priceLo + r.Intn(priceSpan)) }
	randQty := func() types.Qty { return types.Qty(1 + r.Intn(maxQty)) }

	var orders []types.Command
	issued := []types.OrderID{}
	var id types.OrderID = 1000
	for i := 0; i < n; i++ {
		roll := r.Intn(100)
		switch {
		case len(issued) > 0 && roll < 10:
			// Cancel a previously-issued order (no-op if already gone — idempotent).
			orders = append(orders, types.Command{Type: types.CmdCancel, OrderID: issued[r.Intn(len(issued))]})
		case len(issued) > 0 && roll < 18:
			// Amend a previously-issued order to a new price/qty.
			orders = append(orders, types.Command{
				Type: types.CmdAmend, OrderID: issued[r.Intn(len(issued))],
				Account: types.AccountID(1 + r.Intn(genAccounts)), Price: randPrice(), Qty: randQty(),
			})
		default:
			id++
			issued = append(issued, id)
			c := types.Command{
				Type: types.CmdNewOrder, Market: genMarkets[r.Intn(len(genMarkets))],
				Account: types.AccountID(1 + r.Intn(genAccounts)), OrderID: id,
				Side: types.Side(r.Intn(2)), Price: randPrice(), Qty: randQty(),
			}
			applyType(&c, r, sharp, randPrice)
			orders = append(orders, c)
		}
	}
	return Stream{Deposits: deposits, Orders: orders, NetDeposits: net}
}

// applyType selects an order type/TIF/flags for a new-order command, weighted to
// exercise all eight types. Market orders drop their price; stops set a trigger.
func applyType(c *types.Command, r *rand.Rand, sharp bool, randPrice func() types.Price) {
	switch n := r.Intn(100); {
	case n < 40:
		c.OrdType, c.Tif = types.Limit, types.GTC
	case n < 55:
		c.OrdType, c.Tif, c.Price = types.Market, types.GTC, 0
	case n < 65:
		c.OrdType, c.Tif = types.Limit, types.IOC
	case n < 75:
		c.OrdType, c.Tif = types.Limit, types.FOK
	case n < 83:
		c.OrdType, c.Tif, c.Flags = types.Limit, types.GTC, types.FlagPostOnly
	case n < 90:
		c.OrdType, c.Tif, c.Flags = types.Limit, types.GTC, types.FlagIceberg
		c.DisplayQty = 1
		if c.Qty > 1 {
			c.DisplayQty = types.Qty(1 + r.Intn(int(c.Qty)))
		}
	case n < 95:
		// Stop -> Market on trigger.
		c.OrdType, c.Tif, c.Price = types.Stop, types.GTC, 0
		c.StopPrice = randPrice()
	default:
		// Stop-Limit -> Limit at Price on trigger.
		c.OrdType, c.Tif = types.StopLimit, types.GTC
		c.StopPrice = randPrice()
	}
}
