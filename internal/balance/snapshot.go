package balance

import (
	"encoding/binary"
	"errors"
	"sort"

	"github.com/yanuar-ar/order-book/internal/types"
)

// ErrBadLedgerSection is returned when a ledger snapshot section is malformed or
// truncated.
var ErrBadLedgerSection = errors.New("balance: bad ledger snapshot section")

// EncodeSnapshot serializes the full ledger state — balances, accumulated fees,
// and the per-order reservation map — into a deterministic byte section for an
// engine snapshot. Entries are sorted by key so that two logically equal ledgers
// produce identical bytes regardless of Go map iteration order.
//
// Reservations are serialized verbatim by key-set, including entries whose
// remaining is 0 (a fully-settled but not-yet-released order keeps a zero
// reservation). Dropping them would orphan the order on restore and make a later
// Release fail silently. Zero-valued balances and fees are omitted because an
// absent key already reads as zero.
//
// Layout (little-endian):
//
//	nBal  uint32, nBal  × { Acct uint64, Asset uint32, Available int64, Reserved int64 }
//	nFee  uint32, nFee  × { Asset uint32, Amount int64 }
//	nRes  uint32, nRes  × { OrderID uint64, Acct uint64, Asset uint32, Remaining int64, Side uint8 }
func (l *Ledger) EncodeSnapshot() []byte {
	bals := make([]key, 0, len(l.bal))
	for k, b := range l.bal {
		if b.Available == 0 && b.Reserved == 0 {
			continue
		}
		bals = append(bals, k)
	}
	sort.Slice(bals, func(i, j int) bool {
		if bals[i].Acct != bals[j].Acct {
			return bals[i].Acct < bals[j].Acct
		}
		return bals[i].Asset < bals[j].Asset
	})

	feeAssets := make([]types.AssetID, 0, len(l.fees))
	for a, amt := range l.fees {
		if amt == 0 {
			continue
		}
		feeAssets = append(feeAssets, a)
	}
	sort.Slice(feeAssets, func(i, j int) bool { return feeAssets[i] < feeAssets[j] })

	resIDs := make([]types.OrderID, 0, len(l.res))
	for id := range l.res {
		resIDs = append(resIDs, id)
	}
	sort.Slice(resIDs, func(i, j int) bool { return resIDs[i] < resIDs[j] })

	buf := make([]byte, 0, 12+len(bals)*28+len(feeAssets)*12+len(resIDs)*29)
	buf = appendU32(buf, uint32(len(bals)))
	for _, k := range bals {
		b := l.bal[k]
		buf = appendU64(buf, uint64(k.Acct))
		buf = appendU32(buf, uint32(k.Asset))
		buf = appendU64(buf, uint64(b.Available))
		buf = appendU64(buf, uint64(b.Reserved))
	}
	buf = appendU32(buf, uint32(len(feeAssets)))
	for _, a := range feeAssets {
		buf = appendU32(buf, uint32(a))
		buf = appendU64(buf, uint64(l.fees[a]))
	}
	buf = appendU32(buf, uint32(len(resIDs)))
	for _, id := range resIDs {
		r := l.res[id]
		buf = appendU64(buf, uint64(id))
		buf = appendU64(buf, uint64(r.acct))
		buf = appendU32(buf, uint32(r.asset))
		buf = appendU64(buf, uint64(r.remaining))
		buf = append(buf, byte(r.side))
	}
	return buf
}

// Restore rebuilds a ledger from a section produced by EncodeSnapshot. The cfg
// (scales, fee rates, market specs) is supplied by the caller — it lives in the
// snapshot header, not the section. Balances, fees, and reservations are set
// directly: Reserve is never called (it would re-round and double-mutate the
// balance), and Available/Reserved/remaining are read as opaque int64s exactly
// as stored.
func Restore(cfg Config, section []byte) (*Ledger, error) {
	l := New(cfg)
	d := decoder{buf: section}

	nBal, ok := d.u32()
	if !ok {
		return nil, ErrBadLedgerSection
	}
	for i := uint32(0); i < nBal; i++ {
		acct, ok1 := d.u64()
		asset, ok2 := d.u32()
		avail, ok3 := d.u64()
		reserved, ok4 := d.u64()
		if !(ok1 && ok2 && ok3 && ok4) {
			return nil, ErrBadLedgerSection
		}
		l.bal[key{types.AccountID(acct), types.AssetID(asset)}] = Balance{
			Available: int64(avail),
			Reserved:  int64(reserved),
		}
	}

	nFee, ok := d.u32()
	if !ok {
		return nil, ErrBadLedgerSection
	}
	for i := uint32(0); i < nFee; i++ {
		asset, ok1 := d.u32()
		amt, ok2 := d.u64()
		if !(ok1 && ok2) {
			return nil, ErrBadLedgerSection
		}
		l.fees[types.AssetID(asset)] = int64(amt)
	}

	nRes, ok := d.u32()
	if !ok {
		return nil, ErrBadLedgerSection
	}
	for i := uint32(0); i < nRes; i++ {
		id, ok1 := d.u64()
		acct, ok2 := d.u64()
		asset, ok3 := d.u32()
		rem, ok4 := d.u64()
		side, ok5 := d.u8()
		if !(ok1 && ok2 && ok3 && ok4 && ok5) {
			return nil, ErrBadLedgerSection
		}
		l.res[types.OrderID(id)] = reservation{
			acct:      types.AccountID(acct),
			asset:     types.AssetID(asset),
			remaining: int64(rem),
			side:      types.Side(side),
		}
	}
	if !d.done() {
		return nil, ErrBadLedgerSection
	}
	return l, nil
}

// ---- little-endian append/decode helpers (shared style with internal/wal) ----

func appendU32(b []byte, v uint32) []byte {
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], v)
	return append(b, t[:]...)
}

func appendU64(b []byte, v uint64) []byte {
	var t [8]byte
	binary.LittleEndian.PutUint64(t[:], v)
	return append(b, t[:]...)
}

type decoder struct {
	buf []byte
	off int
}

func (d *decoder) u8() (uint8, bool) {
	if d.off+1 > len(d.buf) {
		return 0, false
	}
	v := d.buf[d.off]
	d.off++
	return v, true
}

func (d *decoder) u32() (uint32, bool) {
	if d.off+4 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(d.buf[d.off:])
	d.off += 4
	return v, true
}

func (d *decoder) u64() (uint64, bool) {
	if d.off+8 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint64(d.buf[d.off:])
	d.off += 8
	return v, true
}

func (d *decoder) done() bool { return d.off == len(d.buf) }
