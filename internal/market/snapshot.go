package market

import (
	"encoding/binary"
	"errors"
	"sort"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// snapshotVersion is the snapshot format version. Bump it on any change to the
// section layout — the format is a durability contract, like the WAL.
const snapshotVersion uint16 = 1

// Snapshot section indices within the wal container.
const (
	secHeader = iota
	secLedger
	secOpenMap
	secBooks
	secStops
	secCount
)

// ErrBadOpenSection is returned when an open-map snapshot section is malformed or
// truncated.
var ErrBadOpenSection = errors.New("market: bad open-map snapshot section")

// ErrSnapshotIncompatible is returned when a snapshot is structurally readable
// (CRC-clean) but cannot be trusted: wrong format version, a market/asset or
// money-scale config that differs from the running engine, or post-rebuild
// invariants that do not hold (a logically-corrupt snapshot). Recovery treats it
// like ErrBadSnapshot and falls back to full replay.
var ErrSnapshotIncompatible = errors.New("market: incompatible or corrupt snapshot")

// Snapshot captures the engine's complete resumable state at the current Seq and
// writes it via the wal container. It drains to a quiesced command boundary
// first (so no activation is mid-cascade), then forces the WAL durable through
// the watermark so recovery never finds a snapshot ahead of the log.
func (e *Engine) Snapshot(path string) error {
	e.Drain()
	if err := e.SyncJournal(); err != nil {
		return err
	}
	sections := make([][]byte, secCount)
	sections[secHeader] = e.encodeHeader()
	sections[secLedger] = e.core.ledger.EncodeSnapshot()
	sections[secOpenMap] = encodeOpenMap(e.core.open)
	sections[secBooks] = e.encodeBooks()
	sections[secStops] = e.encodeStops()
	return wal.WriteSnapshot(path, uint64(e.seq.Seq()), sections)
}

// Restore rebuilds a fresh engine from the snapshot at path using cfg (the same
// config the engine runs with). It validates the header against cfg, rebuilds
// ledger, books, stops, and the open-map, primes the sequencer watermark, and
// runs a post-rebuild self-check. Stops are suppressed so a subsequent WAL-tail
// replay does not re-trigger journaled activations; the caller re-enables them
// via EnableStops once the tail is applied.
func Restore(cfg Config, path string) (*Engine, error) {
	seq, sections, err := wal.ReadSnapshot(path)
	if err != nil {
		return nil, err // bad CRC / truncated → caller falls back to full replay
	}
	if len(sections) != secCount {
		return nil, ErrSnapshotIncompatible
	}
	if err := validateHeader(sections[secHeader], cfg); err != nil {
		return nil, err
	}

	cfg.SuppressStops = true
	e := NewEngine(cfg)

	led, err := balance.Restore(balanceConfig(cfg), sections[secLedger])
	if err != nil {
		return nil, err
	}
	e.core.ledger = led

	open, err := decodeOpenMap(sections[secOpenMap])
	if err != nil {
		return nil, err
	}
	e.core.open = open

	if err := e.restoreBooks(sections[secBooks]); err != nil {
		return nil, err
	}
	if err := e.restoreStops(sections[secStops]); err != nil {
		return nil, err
	}

	e.SetSeq(types.Seq(seq))

	if err := e.selfCheck(); err != nil {
		return nil, err
	}
	return e, nil
}

// encodeHeader serializes the format version, the money-scale config the ledger
// integers were computed under, and the market→asset layout. Restore rejects any
// mismatch: the serialized balances are integers under the snapshot-time scales,
// so a config change would silently mis-price every restored reservation.
//
// Layout: version u16, qtyScale i64, feeScale i64, makerFee i64, takerFee i64,
// nMarkets u32, nMarkets × { MarketID u32, Base u32, Quote u32 } (sorted).
func (e *Engine) encodeHeader() []byte {
	mids := e.MarketIDs()
	buf := make([]byte, 0, 2+32+4+len(mids)*12)
	buf = openU16(buf, snapshotVersion)
	buf = openU64(buf, uint64(e.cfg.QtyScale))
	buf = openU64(buf, uint64(e.cfg.FeeScale))
	buf = openU64(buf, uint64(e.cfg.MakerFee))
	buf = openU64(buf, uint64(e.cfg.TakerFee))
	buf = openU32(buf, uint32(len(mids)))
	for _, m := range mids {
		spec := e.cfg.Markets[m]
		buf = openU32(buf, uint32(m))
		buf = openU32(buf, uint32(spec.Base))
		buf = openU32(buf, uint32(spec.Quote))
	}
	return buf
}

// validateHeader rejects a snapshot whose format version, money-scale config, or
// market/asset layout differs from the running engine's cfg.
func validateHeader(section []byte, cfg Config) error {
	d := openDecoder{buf: section}
	version, ok := d.u16()
	if !ok || version != snapshotVersion {
		return ErrSnapshotIncompatible
	}
	qty, a := d.u64()
	fee, b := d.u64()
	maker, c := d.u64()
	taker, f := d.u64()
	nMarkets, g := d.u32()
	if !(a && b && c && f && g) {
		return ErrSnapshotIncompatible
	}
	if int64(qty) != cfg.QtyScale || int64(fee) != cfg.FeeScale ||
		int64(maker) != cfg.MakerFee || int64(taker) != cfg.TakerFee {
		return ErrSnapshotIncompatible
	}
	if int(nMarkets) != len(cfg.Markets) {
		return ErrSnapshotIncompatible
	}
	for i := uint32(0); i < nMarkets; i++ {
		mid, ok1 := d.u32()
		base, ok2 := d.u32()
		quote, ok3 := d.u32()
		if !(ok1 && ok2 && ok3) {
			return ErrSnapshotIncompatible
		}
		spec, found := cfg.Markets[types.MarketID(mid)]
		if !found || uint32(spec.Base) != base || uint32(spec.Quote) != quote {
			return ErrSnapshotIncompatible
		}
	}
	if !d.done() {
		return ErrSnapshotIncompatible
	}
	return nil
}

// encodeBooks combines every market's book section, each tagged with its market
// id, in ascending market order. Layout: nMarkets u32, nMarkets × { MarketID u32,
// len u32, bookBytes }.
func (e *Engine) encodeBooks() []byte {
	return e.encodeMarketSections(func(sh *Shard) []byte { return sh.Book().EncodeSnapshot() })
}

// encodeStops combines every market's stop section, same shape as encodeBooks.
func (e *Engine) encodeStops() []byte {
	return e.encodeMarketSections(func(sh *Shard) []byte { return sh.engine.EncodeSnapshot() })
}

func (e *Engine) encodeMarketSections(sectionOf func(*Shard) []byte) []byte {
	mids := e.MarketIDs()
	buf := openU32(nil, uint32(len(mids)))
	for _, m := range mids {
		sec := sectionOf(e.impls[m])
		buf = openU32(buf, uint32(m))
		buf = openU32(buf, uint32(len(sec)))
		buf = append(buf, sec...)
	}
	return buf
}

// marketSections splits a combined section into per-market byte slices, checking
// each market id is known to the engine.
func (e *Engine) marketSections(section []byte) (map[types.MarketID][]byte, error) {
	d := openDecoder{buf: section}
	n, ok := d.u32()
	if !ok {
		return nil, ErrSnapshotIncompatible
	}
	out := make(map[types.MarketID][]byte, n)
	for i := uint32(0); i < n; i++ {
		mid, ok1 := d.u32()
		ln, ok2 := d.u32()
		if !(ok1 && ok2) || d.off+int(ln) > len(d.buf) {
			return nil, ErrSnapshotIncompatible
		}
		if _, known := e.impls[types.MarketID(mid)]; !known {
			return nil, ErrSnapshotIncompatible
		}
		out[types.MarketID(mid)] = d.buf[d.off : d.off+int(ln)]
		d.off += int(ln)
	}
	if !d.done() {
		return nil, ErrSnapshotIncompatible
	}
	return out, nil
}

func (e *Engine) restoreBooks(section []byte) error {
	secs, err := e.marketSections(section)
	if err != nil {
		return err
	}
	for m, bytes := range secs {
		if err := e.impls[m].Book().RestoreSnapshot(bytes); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) restoreStops(section []byte) error {
	secs, err := e.marketSections(section)
	if err != nil {
		return err
	}
	for m, bytes := range secs {
		if err := e.impls[m].engine.RestoreSnapshot(bytes); err != nil {
			return err
		}
	}
	return nil
}

// selfCheck rejects a logically-corrupt (but CRC-clean) snapshot: it runs the
// ledger and book invariants immediately after rebuild. A failure becomes
// ErrSnapshotIncompatible so recovery falls back to full replay rather than
// loading poisoned state.
func (e *Engine) selfCheck() error {
	if err := e.core.ledger.Verify(); err != nil {
		return ErrSnapshotIncompatible
	}
	for _, m := range e.MarketIDs() {
		if err := e.impls[m].Book().Verify(); err != nil {
			return ErrSnapshotIncompatible
		}
	}
	return nil
}

func openU16(b []byte, v uint16) []byte {
	var t [2]byte
	binary.LittleEndian.PutUint16(t[:], v)
	return append(b, t[:]...)
}

func (d *openDecoder) u16() (uint16, bool) {
	if d.off+2 > len(d.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint16(d.buf[d.off:])
	d.off += 2
	return v, true
}

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
