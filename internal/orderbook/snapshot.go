package orderbook

import (
	"encoding/binary"
	"errors"

	"github.com/yanuar-ar/order-book/internal/types"
)

// ErrBadBookSection is returned when a book snapshot section is malformed or
// truncated.
var ErrBadBookSection = errors.New("orderbook: bad book snapshot section")

// RestoredOrder carries every field needed to re-rest an order exactly as it was
// at snapshot time. Unlike RestingDump (a lossy display view), it carries all
// four quantity fields — remaining, display, hidden, peak — because a
// partially-refilled iceberg cannot be reconstructed from remaining+display
// alone (peak is independent of the current display).
type RestoredOrder struct {
	ID        types.OrderID
	Account   types.AccountID
	Side      types.Side
	Price     types.Price
	Remaining types.Qty
	Display   types.Qty
	Hidden    types.Qty
	Peak      types.Qty
	Tif       types.TIF
	Flags     types.Flags
}

// InsertRestored re-rests an order with all four quantity fields set directly,
// appending at the FIFO tail of its level. Unlike Insert, it does not derive
// hidden/peak from Qty/Display — restore supplies them verbatim so a mid-refill
// iceberg keeps its original peak. Resting orders are always Limit-typed.
func (b *Book) InsertRestored(o RestoredOrder) uint32 {
	idx := b.alloc()
	b.arena[idx] = orderNode{
		id:        o.ID,
		account:   o.Account,
		price:     o.Price,
		remaining: o.Remaining,
		display:   o.Display,
		hidden:    o.Hidden,
		peak:      o.Peak,
		side:      o.Side,
		typ:       types.Limit,
		tif:       o.Tif,
		flags:     o.Flags,
		next:      NilIdx,
		prev:      NilIdx,
	}
	lv := b.levelOrCreate(o.Side, o.Price)
	if lv.head == NilIdx {
		lv.head, lv.tail = idx, idx
	} else {
		b.arena[lv.tail].next = idx
		b.arena[idx].prev = lv.tail
		lv.tail = idx
	}
	lv.totalQty += o.Display
	b.idIndex[o.ID] = idx
	return idx
}

// EncodeSnapshot serializes the book's resting orders in deterministic FIFO order
// (bids then asks, ascending price, oldest-first within a level) plus the last
// trade price. Re-inserting in this order via InsertRestored reproduces identical
// time priority, so the encoding does not depend on arena slot identity.
//
// Layout (little-endian):
//
//	lastPrice int64, hasLast uint8, nOrders uint32,
//	nOrders × { ID u64, Account u64, Side u8, Price i64,
//	            Remaining i64, Display i64, Hidden i64, Peak i64, Tif u8, Flags u16 }
func (b *Book) EncodeSnapshot() []byte {
	const recSize = 8 + 8 + 1 + 8 + 8 + 8 + 8 + 8 + 1 + 2
	buf := make([]byte, 0, 13+b.Len()*recSize)
	buf = appendU64(buf, uint64(b.lastPrice))
	if b.hasLast {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	buf = appendU32(buf, uint32(b.Len()))

	emit := func(prices []types.Price, levels map[types.Price]*level) {
		for _, p := range prices {
			lv := levels[p]
			for idx := lv.head; idx != NilIdx; idx = b.arena[idx].next {
				n := b.arena[idx]
				buf = appendU64(buf, uint64(n.id))
				buf = appendU64(buf, uint64(n.account))
				buf = append(buf, byte(n.side))
				buf = appendU64(buf, uint64(n.price))
				buf = appendU64(buf, uint64(n.remaining))
				buf = appendU64(buf, uint64(n.display))
				buf = appendU64(buf, uint64(n.hidden))
				buf = appendU64(buf, uint64(n.peak))
				buf = append(buf, byte(n.tif))
				buf = appendU16(buf, uint16(n.flags))
			}
		}
	}
	emit(b.bidPrices, b.bidLevels)
	emit(b.askPrices, b.askLevels)
	return buf
}

// RestoreSnapshot rebuilds the book from a section produced by EncodeSnapshot.
// The book must be empty. Orders are re-rested in encoded (FIFO) order, then the
// last trade price is restored.
func (b *Book) RestoreSnapshot(section []byte) error {
	d := bookDecoder{buf: section}
	last, ok1 := d.u64()
	hasLast, ok2 := d.u8()
	n, ok3 := d.u32()
	if !(ok1 && ok2 && ok3) {
		return ErrBadBookSection
	}
	for i := uint32(0); i < n; i++ {
		id, a := d.u64()
		acct, b1 := d.u64()
		side, c := d.u8()
		price, e := d.u64()
		rem, f := d.u64()
		disp, g := d.u64()
		hid, h := d.u64()
		peak, k := d.u64()
		tif, m := d.u8()
		flags, p := d.u16()
		if !(a && b1 && c && e && f && g && h && k && m && p) {
			return ErrBadBookSection
		}
		b.InsertRestored(RestoredOrder{
			ID:        types.OrderID(id),
			Account:   types.AccountID(acct),
			Side:      types.Side(side),
			Price:     types.Price(price),
			Remaining: types.Qty(rem),
			Display:   types.Qty(disp),
			Hidden:    types.Qty(hid),
			Peak:      types.Qty(peak),
			Tif:       types.TIF(tif),
			Flags:     types.Flags(flags),
		})
	}
	if !d.done() {
		return ErrBadBookSection
	}
	b.lastPrice = types.Price(last)
	b.hasLast = hasLast == 1
	return nil
}

// ---- little-endian append/decode helpers (shared style with internal/wal) ----

func appendU16(b []byte, v uint16) []byte {
	var t [2]byte
	binary.LittleEndian.PutUint16(t[:], v)
	return append(b, t[:]...)
}

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

type bookDecoder struct {
	buf []byte
	off int
}

func (d *bookDecoder) u8() (uint8, bool) {
	if d.off+1 > len(d.buf) {
		return 0, false
	}
	v := d.buf[d.off]
	d.off++
	return v, true
}

func (d *bookDecoder) u16() (uint16, bool) {
	if d.off+2 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint16(d.buf[d.off:])
	d.off += 2
	return v, true
}

func (d *bookDecoder) u32() (uint32, bool) {
	if d.off+4 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(d.buf[d.off:])
	d.off += 4
	return v, true
}

func (d *bookDecoder) u64() (uint64, bool) {
	if d.off+8 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint64(d.buf[d.off:])
	d.off += 8
	return v, true
}

func (d *bookDecoder) done() bool { return d.off == len(d.buf) }
