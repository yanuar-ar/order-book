package property

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/tests/refmodel"
	"pgregory.net/rapid"
)

// TestEngineStateMachine drives a rapid-generated sequence of commands through
// both the engine and the reference model, asserting state equality and
// CheckAllInvariants after each step. On failure, rapid shrinks to a minimal
// reproducing command sequence.
func TestEngineStateMachine(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		e := market.NewEngine(engineCfg())
		mod := refmodel.New(modelCfg())

		deps, net := standardPrelude()
		for _, c := range deps {
			e.Submit(c)
			mod.Apply(c)
		}
		e.Drain()

		issued := []types.OrderID{}
		var id types.OrderID = 1000
		steps := rapid.IntRange(1, 50).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			c := drawCommand(rt, &id, &issued)
			c.Seq = types.Seq(i + 1)
			if c.Type == types.CmdWithdraw {
				before := e.Ledger().Available(c.Account, c.Asset)
				e.Submit(c)
				e.Drain()
				net[c.Asset] -= before - e.Ledger().Available(c.Account, c.Asset)
			} else {
				e.Submit(c)
				e.Drain()
			}
			mod.Apply(c)

			if eng, ref := engineState(e).Canonical(), mod.Snapshot().Canonical(); eng != ref {
				rt.Fatalf("state diverged (cmd %+v):\n--- engine ---\n%s\n--- model ---\n%s", c, eng, ref)
			}
			if err := CheckAllInvariants(e, net); err != nil {
				rt.Fatalf("invariant violated (cmd %+v): %v", c, err)
			}
		}
	})
}

func drawCommand(rt *rapid.T, id *types.OrderID, issued *[]types.OrderID) types.Command {
	kind := rapid.IntRange(0, 9).Draw(rt, "kind")
	if len(*issued) > 0 && kind == 0 {
		sel := rapid.IntRange(0, len(*issued)-1).Draw(rt, "cancelSel")
		return types.Command{Type: types.CmdCancel, OrderID: (*issued)[sel]}
	}
	if kind == 2 {
		asset := genQuote
		if rapid.Bool().Draw(rt, "wdBase") {
			asset = rapid.SampledFrom(genBases).Draw(rt, "wdAsset")
		}
		return types.Command{
			Type: types.CmdWithdraw, Account: types.AccountID(rapid.IntRange(1, genAccounts).Draw(rt, "wdAcct")),
			Asset: asset, Amount: int64(rapid.IntRange(1, 50).Draw(rt, "wdAmt")),
		}
	}
	if len(*issued) > 0 && kind == 1 {
		sel := rapid.IntRange(0, len(*issued)-1).Draw(rt, "amendSel")
		return types.Command{
			Type: types.CmdAmend, OrderID: (*issued)[sel],
			Account: types.AccountID(rapid.IntRange(1, genAccounts).Draw(rt, "amAcct")),
			Price:   types.Price(rapid.IntRange(98, 102).Draw(rt, "amPrice")),
			Qty:     types.Qty(rapid.IntRange(1, 20).Draw(rt, "amQty")),
		}
	}
	*id++
	*issued = append(*issued, *id)
	c := types.Command{
		Type: types.CmdNewOrder, Market: rapid.SampledFrom(genMarkets).Draw(rt, "mkt"),
		Account: types.AccountID(rapid.IntRange(1, genAccounts).Draw(rt, "acct")), OrderID: *id,
		Side:  types.Side(rapid.IntRange(0, 1).Draw(rt, "side")),
		Price: types.Price(rapid.IntRange(98, 102).Draw(rt, "price")),
		Qty:   types.Qty(rapid.IntRange(1, 20).Draw(rt, "qty")),
	}
	switch rapid.SampledFrom([]string{"limit", "market", "ioc", "fok", "postonly", "iceberg", "stop", "stoplimit"}).Draw(rt, "type") {
	case "market":
		c.OrdType, c.Tif, c.Price = types.Market, types.GTC, 0
	case "ioc":
		c.OrdType, c.Tif = types.Limit, types.IOC
	case "fok":
		c.OrdType, c.Tif = types.Limit, types.FOK
	case "postonly":
		c.OrdType, c.Tif, c.Flags = types.Limit, types.GTC, types.FlagPostOnly
	case "iceberg":
		c.OrdType, c.Tif, c.Flags = types.Limit, types.GTC, types.FlagIceberg
		c.DisplayQty = types.Qty(rapid.IntRange(1, int(c.Qty)).Draw(rt, "display"))
	case "stop":
		c.OrdType, c.Tif, c.Price = types.Stop, types.GTC, 0
		c.StopPrice = types.Price(rapid.IntRange(98, 102).Draw(rt, "stopPrice"))
	case "stoplimit":
		c.OrdType, c.Tif = types.StopLimit, types.GTC
		c.StopPrice = types.Price(rapid.IntRange(98, 102).Draw(rt, "stopLimitTrigger"))
	default:
		c.OrdType, c.Tif = types.Limit, types.GTC
	}
	return c
}
