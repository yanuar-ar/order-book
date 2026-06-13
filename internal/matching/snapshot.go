package matching

import (
	"encoding/binary"
	"errors"
	"sort"

	"github.com/yanuar-ar/order-book/internal/types"
)

// ErrBadStopSection is returned when a stop snapshot section is malformed or
// truncated.
var ErrBadStopSection = errors.New("matching: bad stop snapshot section")

// EncodeSnapshot serializes every pending stop order as its raw FundedOrder, in
// deterministic order (by Seq, then OrderID). The full FundedOrder is carried —
// not the lossy StopView — because Tif, Flags, DisplayQty, and MaxQuote are all
// needed to re-activate the stop exactly as it would under full replay.
//
// Layout (little-endian):
//
//	nStops uint32, nStops × {
//	  Seq u64, Market u32, Account u64, OrderID u64, Side u8, OrdType u8,
//	  Tif u8, Flags u16, Price i64, StopPrice i64, Qty i64, DisplayQty i64, MaxQuote i64 }
func (e *Engine) EncodeSnapshot() []byte {
	ords := make([]types.FundedOrder, len(e.stops))
	for i, s := range e.stops {
		ords[i] = s.ord
	}
	sort.Slice(ords, func(i, j int) bool {
		if ords[i].Seq != ords[j].Seq {
			return ords[i].Seq < ords[j].Seq
		}
		return ords[i].OrderID < ords[j].OrderID
	})

	const recSize = 8 + 4 + 8 + 8 + 1 + 1 + 1 + 2 + 8 + 8 + 8 + 8 + 8
	buf := make([]byte, 0, 4+len(ords)*recSize)
	buf = stopU32(buf, uint32(len(ords)))
	for _, o := range ords {
		buf = stopU64(buf, uint64(o.Seq))
		buf = stopU32(buf, uint32(o.Market))
		buf = stopU64(buf, uint64(o.Account))
		buf = stopU64(buf, uint64(o.OrderID))
		buf = append(buf, byte(o.Side), byte(o.OrdType), byte(o.Tif))
		buf = stopU16(buf, uint16(o.Flags))
		buf = stopU64(buf, uint64(o.Price))
		buf = stopU64(buf, uint64(o.StopPrice))
		buf = stopU64(buf, uint64(o.Qty))
		buf = stopU64(buf, uint64(o.DisplayQty))
		buf = stopU64(buf, uint64(o.MaxQuote))
	}
	return buf
}

// RestoreSnapshot rebuilds the pending-stop table from a section produced by
// EncodeSnapshot. It sets the stop slice directly — no ledger interaction, no
// matching — so it mirrors addStop without re-reserving funds.
func (e *Engine) RestoreSnapshot(section []byte) error {
	d := stopDecoder{buf: section}
	n, ok := d.u32()
	if !ok {
		return ErrBadStopSection
	}
	stops := make([]stopOrder, 0, n)
	for i := uint32(0); i < n; i++ {
		seq, a := d.u64()
		market, b := d.u32()
		acct, c := d.u64()
		id, e2 := d.u64()
		side, f := d.u8()
		ordType, g := d.u8()
		tif, h := d.u8()
		flags, k := d.u16()
		price, m := d.u64()
		stopPrice, p := d.u64()
		qty, q := d.u64()
		displayQty, r := d.u64()
		maxQuote, s := d.u64()
		if !(a && b && c && e2 && f && g && h && k && m && p && q && r && s) {
			return ErrBadStopSection
		}
		stops = append(stops, stopOrder{ord: types.FundedOrder{
			Seq:        types.Seq(seq),
			Market:     types.MarketID(market),
			Account:    types.AccountID(acct),
			OrderID:    types.OrderID(id),
			Side:       types.Side(side),
			OrdType:    types.OrderType(ordType),
			Tif:        types.TIF(tif),
			Flags:      types.Flags(flags),
			Price:      types.Price(price),
			StopPrice:  types.Price(stopPrice),
			Qty:        types.Qty(qty),
			DisplayQty: types.Qty(displayQty),
			MaxQuote:   int64(maxQuote),
		}})
	}
	if !d.done() {
		return ErrBadStopSection
	}
	e.stops = stops
	return nil
}

// ---- little-endian append/decode helpers (shared style with internal/wal) ----

func stopU16(b []byte, v uint16) []byte {
	var t [2]byte
	binary.LittleEndian.PutUint16(t[:], v)
	return append(b, t[:]...)
}

func stopU32(b []byte, v uint32) []byte {
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], v)
	return append(b, t[:]...)
}

func stopU64(b []byte, v uint64) []byte {
	var t [8]byte
	binary.LittleEndian.PutUint64(t[:], v)
	return append(b, t[:]...)
}

type stopDecoder struct {
	buf []byte
	off int
}

func (d *stopDecoder) u8() (uint8, bool) {
	if d.off+1 > len(d.buf) {
		return 0, false
	}
	v := d.buf[d.off]
	d.off++
	return v, true
}

func (d *stopDecoder) u16() (uint16, bool) {
	if d.off+2 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint16(d.buf[d.off:])
	d.off += 2
	return v, true
}

func (d *stopDecoder) u32() (uint32, bool) {
	if d.off+4 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(d.buf[d.off:])
	d.off += 4
	return v, true
}

func (d *stopDecoder) u64() (uint64, bool) {
	if d.off+8 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint64(d.buf[d.off:])
	d.off += 8
	return v, true
}

func (d *stopDecoder) done() bool { return d.off == len(d.buf) }
