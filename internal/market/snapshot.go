package market

import (
	"encoding/binary"
	"errors"
	"sort"

	"github.com/yanuar-ar/order-book/internal/types"
)

// ErrBadOpenSection is returned when an open-map snapshot section is malformed or
// truncated.
var ErrBadOpenSection = errors.New("market: bad open-map snapshot section")

// encodeOpenMap serializes Core.open — the reservation/open-order map — in
// deterministic OrderID order, so map iteration never leaks into the bytes. The
// full openOrder is carried because it cannot be reconstructed from the book: a
// resting order is stored as Limit, losing the Stop/StopLimit distinction that
// amend depends on.
//
// Layout (little-endian):
//
//	nOpen uint32, nOpen × { OrderID u64, Market u32, Account u64, Side u8, OrdType u8, Price i64, Qty i64 }
func encodeOpenMap(open map[types.OrderID]openOrder) []byte {
	ids := make([]types.OrderID, 0, len(open))
	for id := range open {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	const recSize = 8 + 4 + 8 + 1 + 1 + 8 + 8
	buf := make([]byte, 0, 4+len(ids)*recSize)
	buf = openU32(buf, uint32(len(ids)))
	for _, id := range ids {
		o := open[id]
		buf = openU64(buf, uint64(id))
		buf = openU32(buf, uint32(o.market))
		buf = openU64(buf, uint64(o.account))
		buf = append(buf, byte(o.side), byte(o.ordType))
		buf = openU64(buf, uint64(o.price))
		buf = openU64(buf, uint64(o.qty))
	}
	return buf
}

// decodeOpenMap rebuilds the open-order map from a section produced by
// encodeOpenMap.
func decodeOpenMap(section []byte) (map[types.OrderID]openOrder, error) {
	d := openDecoder{buf: section}
	n, ok := d.u32()
	if !ok {
		return nil, ErrBadOpenSection
	}
	open := make(map[types.OrderID]openOrder, n)
	for i := uint32(0); i < n; i++ {
		id, a := d.u64()
		market, b := d.u32()
		acct, c := d.u64()
		side, e := d.u8()
		ordType, f := d.u8()
		price, g := d.u64()
		qty, h := d.u64()
		if !(a && b && c && e && f && g && h) {
			return nil, ErrBadOpenSection
		}
		open[types.OrderID(id)] = openOrder{
			market:  types.MarketID(market),
			account: types.AccountID(acct),
			side:    types.Side(side),
			ordType: types.OrderType(ordType),
			price:   types.Price(price),
			qty:     types.Qty(qty),
		}
	}
	if !d.done() {
		return nil, ErrBadOpenSection
	}
	return open, nil
}

// ---- little-endian append/decode helpers (shared style with internal/wal) ----

func openU32(b []byte, v uint32) []byte {
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], v)
	return append(b, t[:]...)
}

func openU64(b []byte, v uint64) []byte {
	var t [8]byte
	binary.LittleEndian.PutUint64(t[:], v)
	return append(b, t[:]...)
}

type openDecoder struct {
	buf []byte
	off int
}

func (d *openDecoder) u8() (uint8, bool) {
	if d.off+1 > len(d.buf) {
		return 0, false
	}
	v := d.buf[d.off]
	d.off++
	return v, true
}

func (d *openDecoder) u32() (uint32, bool) {
	if d.off+4 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(d.buf[d.off:])
	d.off += 4
	return v, true
}

func (d *openDecoder) u64() (uint64, bool) {
	if d.off+8 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint64(d.buf[d.off:])
	d.off += 8
	return v, true
}

func (d *openDecoder) done() bool { return d.off == len(d.buf) }
