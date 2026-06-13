package balance

import (
	"strings"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

func verifyTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(Config{
		QtyScale: 1, FeeScale: 100, MakerFee: 1, TakerFee: 2,
		Markets: map[types.MarketID]MarketSpec{0: {Base: 1, Quote: 2}},
	})
	l.Deposit(1, 2, 1000) // quote
	l.Deposit(1, 1, 100)  // base
	// Buy limit: notional 5*3=15, taker fee ceil(15*2/100)=1, reserve 16 quote.
	if reason, ok := l.Reserve(types.FundedOrder{Market: 0, Account: 1, OrderID: 10, Side: types.Buy, OrdType: types.Limit, Price: 5, Qty: 3}); !ok {
		t.Fatalf("buy reserve rejected: %v", reason)
	}
	// Sell: reserve 4 base.
	if reason, ok := l.Reserve(types.FundedOrder{Market: 0, Account: 1, OrderID: 11, Side: types.Sell, Qty: 4}); !ok {
		t.Fatalf("sell reserve rejected: %v", reason)
	}
	return l
}

// ---- Positive ----

func TestLedgerVerify_ConsistentPasses(t *testing.T) {
	l := verifyTestLedger(t)
	if err := l.Verify(); err != nil {
		t.Fatalf("consistent ledger failed Verify: %v", err)
	}
}

func TestReservedOrders_ReturnsSortedIDs(t *testing.T) {
	l := verifyTestLedger(t)
	got := l.ReservedOrders()
	if len(got) != 2 || got[0] != 10 || got[1] != 11 {
		t.Fatalf("ReservedOrders = %v, want [10 11]", got)
	}
}

// ---- Edge ----

func TestLedgerVerify_EmptyPasses(t *testing.T) {
	l := New(Config{QtyScale: 1, FeeScale: 100, Markets: map[types.MarketID]MarketSpec{0: {Base: 1, Quote: 2}}})
	if err := l.Verify(); err != nil {
		t.Fatalf("empty ledger failed Verify: %v", err)
	}
	if len(l.ReservedOrders()) != 0 {
		t.Fatalf("empty ledger has reservations: %v", l.ReservedOrders())
	}
}

func TestLedgerVerify_AfterReleasePasses(t *testing.T) {
	l := verifyTestLedger(t)
	l.Release(10)
	l.Release(11)
	if err := l.Verify(); err != nil {
		t.Fatalf("after release, Verify failed: %v", err)
	}
	if r := l.Reserved(1, 2); r != 0 {
		t.Fatalf("quote still reserved after release: %d", r)
	}
	if len(l.ReservedOrders()) != 0 {
		t.Fatalf("reservations remain after release: %v", l.ReservedOrders())
	}
}

// ---- Negative ----

func TestLedgerVerify_DetectsCorruption(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(l *Ledger)
		wantID string
	}{
		{"negative available", func(l *Ledger) {
			l.bal[key{1, 2}] = Balance{Available: -1, Reserved: 16}
		}, "INV-BAL-01"},
		{"negative reserved", func(l *Ledger) {
			b := l.bal[key{1, 1}]
			b.Reserved = -1
			l.bal[key{1, 1}] = b
		}, "INV-BAL-01"},
		{"negative fee", func(l *Ledger) {
			l.fees[2] = -5
		}, "INV-BAL-01"},
		{"reserved exceeds reservation records", func(l *Ledger) {
			b := l.bal[key{1, 2}]
			b.Reserved += 100 // reserved balance no longer matches res records
			l.bal[key{1, 2}] = b
		}, "INV-BAL-03"},
		{"negative reservation remaining", func(l *Ledger) {
			r := l.res[10]
			r.remaining = -1
			l.res[10] = r
		}, "INV-BAL-03"},
		{"reservation without backing balance", func(l *Ledger) {
			l.res[99] = reservation{acct: 7, asset: 2, remaining: 5, side: types.Buy}
		}, "INV-BAL-03"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := verifyTestLedger(t)
			if err := l.Verify(); err != nil {
				t.Fatalf("precondition: ledger should start valid, got %v", err)
			}
			tc.break_(l)
			err := l.Verify()
			if err == nil {
				t.Fatalf("expected %s violation, got nil", tc.wantID)
			}
			if !strings.Contains(err.Error(), tc.wantID) {
				t.Fatalf("expected %s, got: %v", tc.wantID, err)
			}
		})
	}
}
