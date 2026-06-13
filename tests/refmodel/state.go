package refmodel

import (
	"sort"
	"strings"

	"github.com/yanuar-ar/order-book/internal/types"
)

// Bal is one account|asset balance in a canonical snapshot.
type Bal struct {
	Acct      types.AccountID
	Asset     types.AssetID
	Available int64
	Reserved  int64
}

// Fee is one asset's accumulated fee balance.
type Fee struct {
	Asset  types.AssetID
	Amount int64
}

// Order is one resting order in a canonical snapshot. Hidden iceberg quantity is
// included in Remaining but not Display, mirroring the engine's RestingDump.
type Order struct {
	Market    types.MarketID
	Side      types.Side
	Price     types.Price
	ID        types.OrderID
	Remaining types.Qty
	Display   types.Qty
}

// State is a canonical, layout-independent snapshot of engine or model state.
// Two logically equal states render to the same Canonical() string regardless
// of map order or physical arena layout, so the differential loop compares the
// strings directly.
type State struct {
	Bals   []Bal
	Fees   []Fee
	Orders []Order
}

// Canonical renders the state to a deterministic string. Non-zero balances and
// fees are sorted; orders are sorted by (market, side, price, id).
func (s State) Canonical() string {
	bals := append([]Bal(nil), s.Bals...)
	bals = filterZeroBals(bals)
	sort.Slice(bals, func(i, j int) bool {
		if bals[i].Acct != bals[j].Acct {
			return bals[i].Acct < bals[j].Acct
		}
		return bals[i].Asset < bals[j].Asset
	})
	fees := append([]Fee(nil), s.Fees...)
	fees = filterZeroFees(fees)
	sort.Slice(fees, func(i, j int) bool { return fees[i].Asset < fees[j].Asset })
	orders := append([]Order(nil), s.Orders...)
	sort.Slice(orders, func(i, j int) bool {
		a, b := orders[i], orders[j]
		switch {
		case a.Market != b.Market:
			return a.Market < b.Market
		case a.Side != b.Side:
			return a.Side < b.Side
		case a.Price != b.Price:
			return a.Price < b.Price
		default:
			return a.ID < b.ID
		}
	})

	var sb strings.Builder
	for _, b := range bals {
		writeLine(&sb, "B", int64(b.Acct), int64(b.Asset), b.Available, b.Reserved)
	}
	for _, f := range fees {
		writeLine(&sb, "F", int64(f.Asset), f.Amount)
	}
	for _, o := range orders {
		writeLine(&sb, "O", int64(o.Market), int64(o.Side), int64(o.ID), int64(o.Price), int64(o.Remaining), int64(o.Display))
	}
	return sb.String()
}

func filterZeroBals(in []Bal) []Bal {
	out := in[:0]
	for _, b := range in {
		if b.Available != 0 || b.Reserved != 0 {
			out = append(out, b)
		}
	}
	return out
}

func filterZeroFees(in []Fee) []Fee {
	out := in[:0]
	for _, f := range in {
		if f.Amount != 0 {
			out = append(out, f)
		}
	}
	return out
}

func writeLine(sb *strings.Builder, tag string, vals ...int64) {
	sb.WriteString(tag)
	for _, v := range vals {
		sb.WriteByte(' ')
		sb.WriteString(itoa(v))
	}
	sb.WriteByte('\n')
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
