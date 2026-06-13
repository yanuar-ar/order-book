// Package balance implements the shared balance authority: a single-writer
// ledger of available/reserved funds per account|asset, with maker/taker fees
// credited to a per-asset fee account.
//
// Determinism: the ledger consumes one tagged event stream (BalanceEvent) so
// reservations (in Seq order) and settlements (in fill order) interleave in a
// single fixed order. Reservation rounds up so it never under-covers the
// eventual fill; settlement rounds down.
package balance

import "github.com/yanuar-ar/order-book/internal/types"

// Balance holds the available and reserved amounts for one account|asset.
type Balance struct {
	Available int64
	Reserved  int64
}

type key struct {
	Acct  types.AccountID
	Asset types.AssetID
}

// MarketSpec maps a market to its base and quote assets.
type MarketSpec struct {
	Base  types.AssetID
	Quote types.AssetID
}

// Config carries the scales, fee rates, and market specs the ledger needs.
type Config struct {
	QtyScale int64
	FeeScale int64
	MakerFee int64 // rate at FeeScale, >= 0
	TakerFee int64 // rate at FeeScale, >= 0
	Markets  map[types.MarketID]MarketSpec
}

// reservation records funds locked for one open order so leftovers can be
// released when the order completes or is cancelled.
type reservation struct {
	acct      types.AccountID
	asset     types.AssetID
	remaining int64 // remaining reserved amount (quote for buy, base qty for sell)
	side      types.Side
}

// Ledger is the balance authority.
type Ledger struct {
	cfg  Config
	bal  map[key]Balance
	fees map[types.AssetID]int64
	res  map[types.OrderID]*reservation
}

// New returns an empty ledger.
func New(cfg Config) *Ledger {
	return &Ledger{
		cfg:  cfg,
		bal:  make(map[key]Balance, 1024),
		fees: make(map[types.AssetID]int64),
		res:  make(map[types.OrderID]*reservation, 1024),
	}
}

// Available returns the available balance for an account|asset.
func (l *Ledger) Available(a types.AccountID, asset types.AssetID) int64 {
	return l.bal[key{a, asset}].Available
}

// Reserved returns the reserved balance for an account|asset.
func (l *Ledger) Reserved(a types.AccountID, asset types.AssetID) int64 {
	return l.bal[key{a, asset}].Reserved
}

// Fees returns the accumulated fee balance for an asset.
func (l *Ledger) Fees(asset types.AssetID) int64 { return l.fees[asset] }

func (l *Ledger) move(a types.AccountID, asset types.AssetID, dAvail, dRes int64) {
	k := key{a, asset}
	b := l.bal[k]
	b.Available += dAvail
	b.Reserved += dRes
	l.bal[k] = b
}

// Deposit credits available funds. amount must be > 0.
func (l *Ledger) Deposit(a types.AccountID, asset types.AssetID, amount int64) bool {
	if amount <= 0 {
		return false
	}
	l.move(a, asset, amount, 0)
	return true
}

// Withdraw debits available funds, rejecting when insufficient.
func (l *Ledger) Withdraw(a types.AccountID, asset types.AssetID, amount int64) bool {
	if amount <= 0 || l.Available(a, asset) < amount {
		return false
	}
	l.move(a, asset, -amount, 0)
	return true
}

// Reserve locks funds for a new order. For a limit buy it reserves
// notional + worst-case taker fee (rounded up); for a market buy it reserves
// the account's entire available quote (no price bound); for a sell it reserves
// the base quantity. Returns the reason on rejection.
func (l *Ledger) Reserve(o types.FundedOrder) (types.RejectReason, bool) {
	if _, dup := l.res[o.OrderID]; dup {
		return types.ReasonUnknownOrder, false
	}
	spec := l.cfg.Markets[o.Market]
	if o.Side == types.Buy {
		var cost int64
		if o.OrdType == types.Market {
			cost = l.Available(o.Account, spec.Quote)
			if cost <= 0 {
				return types.ReasonInsufficientFunds, false
			}
		} else {
			notional, ok := types.Notional(o.Price, o.Qty, l.cfg.QtyScale, true)
			if !ok {
				return types.ReasonOverflow, false
			}
			fee, ok := types.Fee(notional, l.cfg.TakerFee, l.cfg.FeeScale, true)
			if !ok {
				return types.ReasonOverflow, false
			}
			cost = notional + fee
		}
		if l.Available(o.Account, spec.Quote) < cost {
			return types.ReasonInsufficientFunds, false
		}
		l.move(o.Account, spec.Quote, -cost, cost)
		l.res[o.OrderID] = &reservation{acct: o.Account, asset: spec.Quote, remaining: cost, side: types.Buy}
		return types.ReasonNone, true
	}

	// Sell: reserve the base quantity.
	need := int64(o.Qty)
	if l.Available(o.Account, spec.Base) < need {
		return types.ReasonInsufficientFunds, false
	}
	l.move(o.Account, spec.Base, -need, need)
	l.res[o.OrderID] = &reservation{acct: o.Account, asset: spec.Base, remaining: need, side: types.Sell}
	return types.ReasonNone, true
}

// Settle applies one fill: the buyer pays quote (notional + buyer fee) from
// reserved and receives base; the seller delivers base from reserved and
// receives quote (notional - seller fee). Both fees are credited to the quote
// fee account. The maker/taker rate per side is derived from f.Taker.
func (l *Ledger) Settle(f types.Fill) {
	spec := l.cfg.Markets[f.Market]
	notional, _ := types.Notional(f.Price, f.Qty, l.cfg.QtyScale, false)

	buyerRate, sellerRate := l.cfg.MakerFee, l.cfg.TakerFee
	if f.Taker == types.Buy {
		buyerRate, sellerRate = l.cfg.TakerFee, l.cfg.MakerFee
	}
	buyerFee, _ := types.Fee(notional, buyerRate, l.cfg.FeeScale, false)
	sellerFee, _ := types.Fee(notional, sellerRate, l.cfg.FeeScale, false)

	// Buyer: reserved quote down by notional+fee, base up by qty.
	l.move(f.BuyAccount, spec.Quote, 0, -(notional + buyerFee))
	l.move(f.BuyAccount, spec.Base, int64(f.Qty), 0)
	if r := l.res[f.BuyOrder]; r != nil {
		r.remaining -= notional + buyerFee
	}

	// Seller: reserved base down by qty, quote up by proceeds (notional-fee).
	l.move(f.SellAccount, spec.Base, 0, -int64(f.Qty))
	l.move(f.SellAccount, spec.Quote, notional-sellerFee, 0)
	if r := l.res[f.SellOrder]; r != nil {
		r.remaining -= int64(f.Qty)
	}

	l.fees[spec.Quote] += buyerFee + sellerFee
}

// AmendReduce shrinks an open order's reservation to match a reduced quantity,
// releasing the freed funds to available. It recomputes the new requirement
// (notional + worst-case taker fee for a buy; base qty for a sell) and releases
// only the excess, so the remaining reservation never under-covers. newQty must
// be the order's new total remaining quantity.
func (l *Ledger) AmendReduce(orderID types.OrderID, side types.Side, price types.Price, newQty types.Qty) bool {
	r, ok := l.res[orderID]
	if !ok {
		return false
	}
	var newReq int64
	if side == types.Buy {
		notional, ok := types.Notional(price, newQty, l.cfg.QtyScale, true)
		if !ok {
			return false
		}
		fee, _ := types.Fee(notional, l.cfg.TakerFee, l.cfg.FeeScale, true)
		newReq = notional + fee
	} else {
		newReq = int64(newQty)
	}
	if r.remaining > newReq {
		rel := r.remaining - newReq
		l.move(r.acct, r.asset, rel, -rel)
		r.remaining = newReq
	}
	return true
}

// Release returns an order's leftover reservation to available funds, used when
// the order completes (fully filled, or remainder cancelled) or is cancelled.
func (l *Ledger) Release(orderID types.OrderID) bool {
	r, ok := l.res[orderID]
	if !ok {
		return false
	}
	if r.remaining > 0 {
		l.move(r.acct, r.asset, r.remaining, -r.remaining)
	}
	delete(l.res, orderID)
	return true
}
