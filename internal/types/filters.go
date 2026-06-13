package types

// MarketFilters is the per-market static order-filter set (CEX-style), enforced
// at submit time. Values are integer fixed-point at the engine scales: prices at
// PriceScale, quantities at QtyScale, and notional bounds as quote values at
// PriceScale (as produced by Notional). The set is the engine-internal mirror of
// pkg/config.FilterSpec; the validation logic lives here so the engine, the
// reference model, and the invariant checker all call the exact same code and
// cannot drift.
//
// Two distinct surfaces use these filters:
//   - ValidateNew enforces the full set (tick, lot, min/max, notional) at submit
//     time, against the order's full quantity.
//   - RestingViolation enforces only the always-true resting properties (price
//     on-tick and in range, remaining/display on-step). Minimum-quantity and
//     minimum-notional are deliberately NOT resting invariants: a partial fill
//     can legitimately leave a remainder below the minimums, exactly as on a
//     real exchange.
type MarketFilters struct {
	// Price filter (limit price and stop trigger/limit prices).
	TickSize int64
	MinPrice int64
	MaxPrice int64

	// Lot filter (limit and stop-limit quantities, plus iceberg total/display).
	StepSize int64
	MinQty   int64
	MaxQty   int64

	// Market lot filter (market and stop-market quantities).
	MktStepSize int64
	MktMinQty   int64
	MktMaxQty   int64

	// Notional filter (quote value = Notional(price, qty)).
	MinNotional int64
	MaxNotional int64
}

// priceOK reports whether p is on-tick and within [MinPrice, MaxPrice].
func (f MarketFilters) priceOK(p Price) bool {
	v := int64(p)
	return v >= f.MinPrice && v <= f.MaxPrice && v%f.TickSize == 0
}

// lotOK reports whether q satisfies the limit lot filter (on-step, in range).
func (f MarketFilters) lotOK(q Qty) bool {
	v := int64(q)
	return v >= f.MinQty && v <= f.MaxQty && v%f.StepSize == 0
}

// mktLotOK reports whether q satisfies the market lot filter.
func (f MarketFilters) mktLotOK(q Qty) bool {
	v := int64(q)
	return v >= f.MktMinQty && v <= f.MktMaxQty && v%f.MktStepSize == 0
}

// notionalInRange reports whether a computed notional is within bounds.
func (f MarketFilters) notionalInRange(notional int64) bool {
	return notional >= f.MinNotional && notional <= f.MaxNotional
}

// checkNotional computes price*qty/qtyScale (truncated) and returns ReasonNotional
// when it is out of range or overflows; otherwise ReasonNone.
func (f MarketFilters) checkNotional(price Price, qty Qty, qtyScale int64) RejectReason {
	n, ok := Notional(price, qty, qtyScale, false)
	if !ok || !f.notionalInRange(n) {
		return ReasonNotional
	}
	return ReasonNone
}

// ValidateNew returns ReasonNone if the order passes every applicable filter,
// otherwise the per-group reject reason. lastPrice/hasLast supply the reference
// price for market and stop-market notional; when hasLast is false that notional
// check is skipped (fail-open, since no trade has set a reference yet).
func (f MarketFilters) ValidateNew(o FundedOrder, qtyScale int64, lastPrice Price, hasLast bool) RejectReason {
	switch o.OrdType {
	case Market:
		if !f.mktLotOK(o.Qty) {
			return ReasonMarketLotSize
		}
		if !hasLast {
			return ReasonNone
		}
		return f.checkNotional(lastPrice, o.Qty, qtyScale)
	case Stop:
		// Stop-market: trigger price on-tick/in-range, market lot, notional via ref.
		if !f.priceOK(o.StopPrice) {
			return ReasonPriceFilter
		}
		if !f.mktLotOK(o.Qty) {
			return ReasonMarketLotSize
		}
		if !hasLast {
			return ReasonNone
		}
		return f.checkNotional(lastPrice, o.Qty, qtyScale)
	case StopLimit:
		// Trigger and limit prices both on-tick/in-range; limit lot; notional on limit.
		if !f.priceOK(o.StopPrice) || !f.priceOK(o.Price) {
			return ReasonPriceFilter
		}
		if r := f.checkLotAndDisplay(o); r != ReasonNone {
			return r
		}
		return f.checkNotional(o.Price, o.Qty, qtyScale)
	default: // Limit
		if !f.priceOK(o.Price) {
			return ReasonPriceFilter
		}
		if r := f.checkLotAndDisplay(o); r != ReasonNone {
			return r
		}
		return f.checkNotional(o.Price, o.Qty, qtyScale)
	}
}

// checkLotAndDisplay validates the total quantity against the limit lot filter
// and, for an iceberg with a partial display, that the visible slice is itself a
// valid lot (on-step and >= MinQty), so no dust slice ever shows on the book.
func (f MarketFilters) checkLotAndDisplay(o FundedOrder) RejectReason {
	if !f.lotOK(o.Qty) {
		return ReasonLotSize
	}
	if o.Flags.Has(FlagIceberg) && o.DisplayQty > 0 && o.DisplayQty < o.Qty {
		d := int64(o.DisplayQty)
		if d%f.StepSize != 0 || d < f.MinQty {
			return ReasonLotSize
		}
	}
	return ReasonNone
}

// ValidateAmendDown re-validates an in-place quantity reduction: the new resting
// quantity must remain a valid lot and the order's notional at its (unchanged)
// price must stay within bounds. Price is unchanged so the price filter is
// unaffected. Returns ReasonNone when the reduced order is still valid.
func (f MarketFilters) ValidateAmendDown(price Price, newQty Qty, qtyScale int64) RejectReason {
	if !f.lotOK(newQty) {
		return ReasonLotSize
	}
	return f.checkNotional(price, newQty, qtyScale)
}

// RestingViolation returns a human-readable description when a resting order
// violates the always-true resting properties (price on-tick and in range,
// remaining and display on-step and not above the lot ceiling), or "" when the
// order is on-grid. Minimums are intentionally excluded — a partial fill may
// leave a remainder below MinQty/MinNotional, which is valid resting state.
func (f MarketFilters) RestingViolation(price Price, remaining, display Qty) string {
	pv := int64(price)
	if pv%f.TickSize != 0 || pv < f.MinPrice || pv > f.MaxPrice {
		return "price off-tick or out of range"
	}
	rv := int64(remaining)
	if rv%f.StepSize != 0 || rv > f.MaxQty {
		return "remaining off-step or above max"
	}
	if display > 0 && display < remaining && int64(display)%f.StepSize != 0 {
		return "display off-step"
	}
	return ""
}
