package types

import "testing"

// baseFilters is a small, readable filter set used across the filter tests.
// qtyScale is 1 in these tests, so Notional(price, qty) == price*qty.
func baseFilters() MarketFilters {
	return MarketFilters{
		TickSize: 10, MinPrice: 100, MaxPrice: 1000,
		StepSize: 5, MinQty: 10, MaxQty: 1000,
		MktStepSize: 5, MktMinQty: 10, MktMaxQty: 1000,
		MinNotional: 1000, MaxNotional: 1_000_000,
	}
}

const testQtyScale = int64(1)

func TestValidateNew(t *testing.T) {
	f := baseFilters()
	hi := f
	hi.MinNotional = 2000 // for the below-min-notional case

	cases := []struct {
		name    string
		filter  MarketFilters
		ord     FundedOrder
		lastP   Price
		hasLast bool
		want    RejectReason
	}{
		// --- Positive ---
		{"limit valid", f, FundedOrder{OrdType: Limit, Price: 100, Qty: 10}, 0, false, ReasonNone},
		{"limit at exact min notional", f, FundedOrder{OrdType: Limit, Price: 100, Qty: 10}, 0, false, ReasonNone},
		{"limit at exact max qty", f, FundedOrder{OrdType: Limit, Price: 100, Qty: 1000}, 0, false, ReasonNone},
		{"market valid with ref", f, FundedOrder{OrdType: Market, Qty: 10}, 100, true, ReasonNone},
		{"market no ref fails open", hi, FundedOrder{OrdType: Market, Qty: 10}, 0, false, ReasonNone},
		{"stop-market valid", f, FundedOrder{OrdType: Stop, StopPrice: 100, Qty: 10}, 100, true, ReasonNone},
		{"stop-limit valid", f, FundedOrder{OrdType: StopLimit, StopPrice: 100, Price: 100, Qty: 10}, 0, false, ReasonNone},
		{"iceberg valid display", f, FundedOrder{OrdType: Limit, Flags: FlagIceberg, Price: 100, Qty: 100, DisplayQty: 10}, 0, false, ReasonNone},

		// --- Negative: price ---
		{"limit off-tick", f, FundedOrder{OrdType: Limit, Price: 105, Qty: 10}, 0, false, ReasonPriceFilter},
		{"limit below min price", f, FundedOrder{OrdType: Limit, Price: 90, Qty: 10}, 0, false, ReasonPriceFilter},
		{"limit above max price", f, FundedOrder{OrdType: Limit, Price: 1010, Qty: 10}, 0, false, ReasonPriceFilter},
		{"stop-market off-tick trigger", f, FundedOrder{OrdType: Stop, StopPrice: 105, Qty: 10}, 100, true, ReasonPriceFilter},
		{"stop-limit off-tick trigger", f, FundedOrder{OrdType: StopLimit, StopPrice: 105, Price: 100, Qty: 10}, 0, false, ReasonPriceFilter},
		{"stop-limit off-tick limit", f, FundedOrder{OrdType: StopLimit, StopPrice: 100, Price: 105, Qty: 10}, 0, false, ReasonPriceFilter},

		// --- Negative: lot ---
		{"limit off-step qty", f, FundedOrder{OrdType: Limit, Price: 100, Qty: 12}, 0, false, ReasonLotSize},
		{"limit below min qty", f, FundedOrder{OrdType: Limit, Price: 100, Qty: 5}, 0, false, ReasonLotSize},
		{"limit above max qty", f, FundedOrder{OrdType: Limit, Price: 100, Qty: 1005}, 0, false, ReasonLotSize},
		{"iceberg display off-step", f, FundedOrder{OrdType: Limit, Flags: FlagIceberg, Price: 100, Qty: 100, DisplayQty: 7}, 0, false, ReasonLotSize},
		{"iceberg display below min", f, FundedOrder{OrdType: Limit, Flags: FlagIceberg, Price: 100, Qty: 100, DisplayQty: 5}, 0, false, ReasonLotSize},

		// --- Negative: market lot ---
		{"market below min", f, FundedOrder{OrdType: Market, Qty: 5}, 100, true, ReasonMarketLotSize},
		{"market off-step", f, FundedOrder{OrdType: Market, Qty: 12}, 100, true, ReasonMarketLotSize},

		// --- Negative: notional ---
		{"limit below min notional", hi, FundedOrder{OrdType: Limit, Price: 100, Qty: 10}, 0, false, ReasonNotional},
		{"market below min notional via ref", hi, FundedOrder{OrdType: Market, Qty: 10}, 100, true, ReasonNotional},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.ValidateNew(tc.ord, testQtyScale, tc.lastP, tc.hasLast); got != tc.want {
				t.Errorf("ValidateNew = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestValidateAmendDown(t *testing.T) {
	f := baseFilters()
	if got := f.ValidateAmendDown(100, 10, testQtyScale); got != ReasonNone {
		t.Errorf("valid amend-down = %d, want ReasonNone", got)
	}
	if got := f.ValidateAmendDown(100, 12, testQtyScale); got != ReasonLotSize {
		t.Errorf("off-step amend-down = %d, want ReasonLotSize", got)
	}
	hi := f
	hi.MinNotional = 2000
	if got := hi.ValidateAmendDown(100, 10, testQtyScale); got != ReasonNotional {
		t.Errorf("below-min-notional amend-down = %d, want ReasonNotional", got)
	}
}

func TestRestingViolation(t *testing.T) {
	f := baseFilters()
	cases := []struct {
		name      string
		price     Price
		remaining Qty
		display   Qty
		wantOK    bool // true = no violation ("")
	}{
		{"on grid", 100, 10, 10, true},
		// A partial fill can leave a remainder below MinQty; that is valid resting
		// state, NOT a filter violation (MinQty is a submit-time check only).
		{"remainder below min qty but on-step", 100, 5, 5, true},
		{"off-tick price", 105, 10, 10, false},
		{"off-step remaining", 100, 7, 7, false},
		{"above max remaining", 100, 1005, 1005, false},
		{"iceberg display off-step", 100, 100, 7, false},
		{"iceberg display on-step", 100, 100, 10, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := f.RestingViolation(tc.price, tc.remaining, tc.display)
			if tc.wantOK && got != "" {
				t.Errorf("RestingViolation = %q, want no violation", got)
			}
			if !tc.wantOK && got == "" {
				t.Errorf("RestingViolation = \"\", want a violation")
			}
		})
	}
}
