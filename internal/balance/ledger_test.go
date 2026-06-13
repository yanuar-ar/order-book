package balance

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	btc  types.AssetID  = 1
	usdt types.AssetID  = 2
	mkt  types.MarketID = 0
)

// newLedger builds a BTC/USDT ledger with QtyScale=1 (so notional = price*qty)
// for arithmetic-clear tests, and the given fee rates over feeScale.
func newLedger(maker, taker, feeScale int64) *Ledger {
	return New(Config{
		QtyScale: 1,
		FeeScale: feeScale,
		MakerFee: maker,
		TakerFee: taker,
		Markets:  map[types.MarketID]MarketSpec{mkt: {Base: btc, Quote: usdt}},
	})
}

func buy(id types.OrderID, acct types.AccountID, typ types.OrderType, price types.Price, qty types.Qty) types.FundedOrder {
	return types.FundedOrder{OrderID: id, Account: acct, Market: mkt, Side: types.Buy, OrdType: typ, Price: price, Qty: qty}
}
func sell(id types.OrderID, acct types.AccountID, qty types.Qty) types.FundedOrder {
	return types.FundedOrder{OrderID: id, Account: acct, Market: mkt, Side: types.Sell, OrdType: types.Limit, Qty: qty}
}

// ---- Positive ----

func TestDepositWithdraw(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 1000)
	if l.Available(1, usdt) != 1000 {
		t.Fatalf("available = %d, want 1000", l.Available(1, usdt))
	}
	if !l.Withdraw(1, usdt, 400) || l.Available(1, usdt) != 600 {
		t.Fatalf("after withdraw = %d, want 600", l.Available(1, usdt))
	}
}

func TestReserveLimitBuyZeroFee(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 100)
	if r, ok := l.Reserve(buy(10, 1, types.Limit, 10, 5)); !ok {
		t.Fatalf("reserve rejected: %v", r)
	}
	if l.Reserved(1, usdt) != 50 || l.Available(1, usdt) != 50 {
		t.Fatalf("balances = avail %d reserved %d, want 50/50", l.Available(1, usdt), l.Reserved(1, usdt))
	}
}

func TestReserveSellLocksBase(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(2, btc, 5)
	if _, ok := l.Reserve(sell(20, 2, 5)); !ok {
		t.Fatal("sell reserve rejected")
	}
	if l.Reserved(2, btc) != 5 || l.Available(2, btc) != 0 {
		t.Fatalf("base = avail %d reserved %d, want 0/5", l.Available(2, btc), l.Reserved(2, btc))
	}
}

func TestSettleMakerTakerFeesAndConservation(t *testing.T) {
	// maker 1%, taker 2% at feeScale 100. notional = 100*10 = 1000.
	l := newLedger(1, 2, 100)
	l.Deposit(1, usdt, 1020) // buyer funds: notional 1000 + taker fee 20
	l.Deposit(2, btc, 10)    // seller funds
	if _, ok := l.Reserve(buy(10, 1, types.Limit, 100, 10)); !ok {
		t.Fatal("buy reserve failed")
	}
	if _, ok := l.Reserve(sell(20, 2, 10)); !ok {
		t.Fatal("sell reserve failed")
	}
	// Buyer is the taker.
	l.Settle(types.Fill{Taker: types.Buy, Market: mkt, Price: 100, Qty: 10,
		BuyOrder: 10, SellOrder: 20, BuyAccount: 1, SellAccount: 2})

	if l.Available(1, btc) != 10 {
		t.Errorf("buyer BTC = %d, want 10", l.Available(1, btc))
	}
	if l.Available(2, usdt) != 990 { // 1000 - 10 maker fee
		t.Errorf("seller USDT = %d, want 990", l.Available(2, usdt))
	}
	if l.Fees(usdt) != 30 { // 20 taker + 10 maker
		t.Errorf("fee account = %d, want 30", l.Fees(usdt))
	}
	l.Release(10)
	l.Release(20)

	// Conservation: total USDT across users + fees must equal initial 1020.
	totalUSDT := l.Available(1, usdt) + l.Reserved(1, usdt) +
		l.Available(2, usdt) + l.Reserved(2, usdt) + l.Fees(usdt)
	if totalUSDT != 1020 {
		t.Errorf("USDT not conserved: %d, want 1020", totalUSDT)
	}
	totalBTC := l.Available(1, btc) + l.Reserved(1, btc) + l.Available(2, btc) + l.Reserved(2, btc)
	if totalBTC != 10 {
		t.Errorf("BTC not conserved: %d, want 10", totalBTC)
	}
}

func TestReleaseReturnsLeftoverReservation(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 100)
	l.Reserve(buy(10, 1, types.Limit, 10, 5)) // reserve 50
	// Settle only part: fill 3 @10 = notional 30, no fee.
	l.Settle(types.Fill{Taker: types.Buy, Market: mkt, Price: 10, Qty: 3,
		BuyOrder: 10, SellOrder: 99, BuyAccount: 1, SellAccount: 2})
	// 50 reserved - 30 settled = 20 leftover.
	l.Release(10)
	if l.Reserved(1, usdt) != 0 {
		t.Errorf("reserved after release = %d, want 0", l.Reserved(1, usdt))
	}
	if l.Available(1, usdt) != 70 { // 50 leftover-avail + 20 released
		t.Errorf("available after release = %d, want 70", l.Available(1, usdt))
	}
}

// ---- Negative ----

func TestWithdrawInsufficientRejects(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 100)
	if l.Withdraw(1, usdt, 200) {
		t.Fatal("over-withdraw should fail")
	}
	if l.Available(1, usdt) != 100 {
		t.Fatalf("balance mutated on failed withdraw: %d", l.Available(1, usdt))
	}
}

func TestReserveBuyInsufficientRejects(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 40)
	if r, ok := l.Reserve(buy(10, 1, types.Limit, 10, 5)); ok || r != types.ReasonInsufficientFunds {
		t.Fatalf("reserve = (%v,%v), want InsufficientFunds", r, ok)
	}
	if l.Reserved(1, usdt) != 0 || l.Available(1, usdt) != 40 {
		t.Fatal("balances mutated on rejected reserve")
	}
}

func TestReserveSellInsufficientBaseRejects(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(2, btc, 3)
	if r, ok := l.Reserve(sell(20, 2, 5)); ok || r != types.ReasonInsufficientFunds {
		t.Fatalf("reserve = (%v,%v), want InsufficientFunds", r, ok)
	}
}

func TestReserveOverflowReported(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 1<<62)
	// price*qty overflows int64 even before the divide.
	r, ok := l.Reserve(buy(10, 1, types.Limit, types.Price(1)<<62, types.Qty(8)))
	if ok || r != types.ReasonOverflow {
		t.Fatalf("reserve = (%v,%v), want Overflow", r, ok)
	}
}

// ---- Edge ----

func TestMarketBuyReservesFullAvailable(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 250)
	if _, ok := l.Reserve(buy(10, 1, types.Market, 0, 5)); !ok {
		t.Fatal("market buy reserve failed")
	}
	if l.Reserved(1, usdt) != 250 || l.Available(1, usdt) != 0 {
		t.Fatalf("market buy reserve = avail %d reserved %d, want 0/250", l.Available(1, usdt), l.Reserved(1, usdt))
	}
}

func TestMarketBuyWithNoFundsRejects(t *testing.T) {
	l := newLedger(0, 0, 100)
	if r, ok := l.Reserve(buy(10, 1, types.Market, 0, 5)); ok || r != types.ReasonInsufficientFunds {
		t.Fatalf("market buy with no funds = (%v,%v), want InsufficientFunds", r, ok)
	}
}

func TestReservationRoundsUpNeverUnderReserves(t *testing.T) {
	// taker rate 1 over feeScale 3 -> fee = notional/3, rounded up at reserve,
	// down at settle. Reserved must be >= settled spend.
	l := newLedger(0, 1, 3)
	l.Deposit(1, usdt, 100)
	l.Reserve(buy(10, 1, types.Limit, 10, 1)) // notional 10, fee up = ceil(10/3)=4 -> reserve 14
	reservedBefore := l.Reserved(1, usdt)
	l.Settle(types.Fill{Taker: types.Buy, Market: mkt, Price: 10, Qty: 1,
		BuyOrder: 10, SellOrder: 99, BuyAccount: 1, SellAccount: 2})
	// settled spend = notional 10 + fee down floor(10/3)=3 = 13 <= 14 reserved.
	spent := reservedBefore - l.Reserved(1, usdt)
	if spent > reservedBefore {
		t.Fatalf("under-reserved: spent %d > reserved %d", spent, reservedBefore)
	}
	if spent != 13 {
		t.Fatalf("settled spend = %d, want 13", spent)
	}
}

func TestUnsettledProceedsNotSpendable(t *testing.T) {
	// Ordering via the event stream: a sell of BTC cannot reserve before the
	// buy fill that delivers that BTC has settled.
	l := newLedger(0, 0, 100)
	// No BTC yet: reserving a sell must fail.
	if _, ok := l.Apply(BalanceEvent{Kind: EvReserve, Order: sell(20, 1, 5)}); ok {
		t.Fatal("sell reserved before BTC delivered")
	}
	// Settle a buy fill delivering 5 BTC to account 1.
	l.Apply(BalanceEvent{Kind: EvSettle, Fill: types.Fill{Taker: types.Buy, Market: mkt, Price: 10, Qty: 5,
		BuyOrder: 10, SellOrder: 99, BuyAccount: 1, SellAccount: 2}})
	// Now the sell can reserve.
	if _, ok := l.Apply(BalanceEvent{Kind: EvReserve, Order: sell(21, 1, 5)}); !ok {
		t.Fatal("sell should reserve after BTC settled")
	}
}

func TestNoNegativeBalanceAcrossSequence(t *testing.T) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 100)
	l.Reserve(buy(10, 1, types.Limit, 10, 5)) // reserve 50
	l.Release(10)                             // release back
	l.Withdraw(1, usdt, 100)                  // drain
	if l.Withdraw(1, usdt, 1) {               // overdraw must fail
		t.Fatal("overdraw succeeded")
	}
	if l.Available(1, usdt) != 0 || l.Reserved(1, usdt) != 0 {
		t.Fatalf("balances = avail %d reserved %d, want 0/0", l.Available(1, usdt), l.Reserved(1, usdt))
	}
}
