package balance

import (
	"sort"

	"github.com/yanuar-ar/order-book/internal/types"
)

// BalDump is one account|asset balance, for deterministic state comparison.
type BalDump struct {
	Acct      types.AccountID
	Asset     types.AssetID
	Available int64
	Reserved  int64
}

// FeeDump is the accumulated fee for one asset.
type FeeDump struct {
	Asset  types.AssetID
	Amount int64
}

// Dump returns all non-zero balances (sorted by account then asset) and all
// non-zero fee balances (sorted by asset). The ordering is canonical so two
// logically equal ledgers produce identical dumps regardless of Go map order.
func (l *Ledger) Dump() ([]BalDump, []FeeDump) {
	bals := make([]BalDump, 0, len(l.bal))
	for k, b := range l.bal {
		if b.Available == 0 && b.Reserved == 0 {
			continue
		}
		bals = append(bals, BalDump{Acct: k.Acct, Asset: k.Asset, Available: b.Available, Reserved: b.Reserved})
	}
	sort.Slice(bals, func(i, j int) bool {
		if bals[i].Acct != bals[j].Acct {
			return bals[i].Acct < bals[j].Acct
		}
		return bals[i].Asset < bals[j].Asset
	})

	fees := make([]FeeDump, 0, len(l.fees))
	for a, amt := range l.fees {
		if amt == 0 {
			continue
		}
		fees = append(fees, FeeDump{Asset: a, Amount: amt})
	}
	sort.Slice(fees, func(i, j int) bool { return fees[i].Asset < fees[j].Asset })
	return bals, fees
}
