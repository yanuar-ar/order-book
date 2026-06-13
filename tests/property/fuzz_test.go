package property

import (
	"bytes"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// decodeStream turns an arbitrary byte slice into a command stream: a funded
// prelude plus orders decoded field-by-field with modulo clamping, so even
// malformed fuzzer input yields a valid, bounded stream. Coverage-guided
// mutation of the bytes explores the order-command space.
func decodeStream(data []byte) Stream {
	deps, net := standardPrelude()
	var orders []types.Command
	issued := []types.OrderID{}
	var id types.OrderID = 1000

	pos := 0
	next := func() byte {
		if pos < len(data) {
			b := data[pos]
			pos++
			return b
		}
		return 0
	}
	for pos < len(data) && len(orders) < 400 {
		op := next()
		switch {
		case op%7 == 0 && len(issued) > 0:
			orders = append(orders, types.Command{Type: types.CmdCancel, OrderID: issued[int(next())%len(issued)]})
		case op%7 == 2:
			asset := genQuote
			if int(op)%2 == 0 {
				asset = genBases[int(next())%len(genBases)]
			}
			orders = append(orders, types.Command{
				Type: types.CmdWithdraw, Account: types.AccountID(1 + int(next())%genAccounts),
				Asset: asset, Amount: int64(1 + int(next())%50),
			})
		case op%7 == 1 && len(issued) > 0:
			sel, pr, q := next(), next(), next()
			orders = append(orders, types.Command{
				Type: types.CmdAmend, OrderID: issued[int(sel)%len(issued)],
				Account: types.AccountID(1 + int(op)%genAccounts),
				Price:   types.Price(90 + int(pr)%21), Qty: types.Qty(1 + int(q)%10),
			})
		default:
			mk, ac, sd, ot, pr, q, dq := next(), next(), next(), next(), next(), next(), next()
			id++
			issued = append(issued, id)
			c := types.Command{
				Type: types.CmdNewOrder, Market: genMarkets[int(mk)%len(genMarkets)],
				Account: types.AccountID(1 + int(ac)%genAccounts), OrderID: id,
				Side: types.Side(int(sd) % 2), Price: types.Price(90 + int(pr)%21), Qty: types.Qty(1 + int(q)%10),
			}
			decodeType(&c, ot, dq)
			orders = append(orders, c)
		}
	}
	return Stream{Deposits: deps, Orders: orders, NetDeposits: net}
}

// decodeType maps a byte to an order type/TIF/flags, covering all eight types.
func decodeType(c *types.Command, ot, dq byte) {
	switch ot % 10 {
	case 0, 1, 2, 3:
		c.OrdType, c.Tif = types.Limit, types.GTC
	case 4:
		c.OrdType, c.Tif, c.Price = types.Market, types.GTC, 0
	case 5:
		c.OrdType, c.Tif = types.Limit, types.IOC
	case 6:
		c.OrdType, c.Tif = types.Limit, types.FOK
	case 7:
		c.OrdType, c.Tif, c.Flags = types.Limit, types.GTC, types.FlagPostOnly
	case 8:
		c.OrdType, c.Tif, c.Flags = types.Limit, types.GTC, types.FlagIceberg
		c.DisplayQty = 1
		if c.Qty > 1 {
			c.DisplayQty = types.Qty(1 + int(dq)%int(c.Qty))
		}
	default:
		c.OrdType, c.Tif, c.Price = types.Stop, types.GTC, 0
		c.StopPrice = types.Price(90 + int(dq)%21)
	}
}

// FuzzEngine drives the differential loop on a decoded byte stream: the engine
// and reference model must agree and all invariants must hold for every input.
// Run: go test -run '^$' -fuzz=FuzzEngine ./tests/property/
func FuzzEngine(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{2, 1, 0, 0, 1, 100, 5, 0, 2, 1, 1, 0, 3, 100, 5, 0})
	f.Add(bytes.Repeat([]byte{3, 2, 1, 0, 5, 100, 2, 0}, 12))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			data = data[:4096]
		}
		if err := RunDifferential(decodeStream(data)); err != nil {
			t.Fatal(err)
		}
	})
}
