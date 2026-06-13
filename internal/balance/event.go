package balance

import "github.com/yanuar-ar/order-book/internal/types"

// EventKind tags a BalanceEvent.
type EventKind uint8

const (
	EvReserve EventKind = iota
	EvSettle
	EvRelease
	EvDeposit
	EvWithdraw
)

// BalanceEvent is the single tagged input the ledger consumes. Routing
// reservations and settlements through one stream keeps their interleaving
// fixed and deterministic (a buyer cannot spend proceeds that have not yet
// settled at an earlier position in the stream).
type BalanceEvent struct {
	Kind    EventKind
	Order   types.FundedOrder // EvReserve
	Fill    types.Fill        // EvSettle
	OrderID types.OrderID     // EvRelease
	Account types.AccountID   // EvDeposit / EvWithdraw
	Asset   types.AssetID     // EvDeposit / EvWithdraw
	Amount  int64             // EvDeposit / EvWithdraw
}

// Apply dispatches one event. For EvReserve it returns the rejection reason and
// success; other kinds return (ReasonNone, ok) where ok reflects whether the
// operation succeeded (e.g., sufficient funds for withdraw).
func (l *Ledger) Apply(ev BalanceEvent) (types.RejectReason, bool) {
	switch ev.Kind {
	case EvReserve:
		return l.Reserve(ev.Order)
	case EvSettle:
		l.Settle(ev.Fill)
		return types.ReasonNone, true
	case EvRelease:
		return types.ReasonNone, l.Release(ev.OrderID)
	case EvDeposit:
		return types.ReasonNone, l.Deposit(ev.Account, ev.Asset, ev.Amount)
	case EvWithdraw:
		return types.ReasonNone, l.Withdraw(ev.Account, ev.Asset, ev.Amount)
	default:
		return types.ReasonNone, false
	}
}
