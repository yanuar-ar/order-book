// Package wal is the single-node write-ahead log: an append-only, segmented,
// CRC-framed journal of records with a torn-tail- and gap-safe replay.
//
// v1 uses buffered os.File I/O with Sync-based group commit; the mmap fast path
// from the design is deferred to the performance phase. The framing, segment,
// and replay semantics (the parts correctness depends on) are independent of
// that choice.
package wal

import (
	"encoding/binary"
	"hash/crc32"
)

// headerSize is the fixed record header length in bytes.
//
//	[0:8]   Seq        uint64
//	[8:16]  TsNanos    int64
//	[16:18] Type       uint16
//	[18:20] Flags      uint16
//	[20:24] PayloadLen uint32
//	[24:28] CRC32      uint32 (over payload)
const headerSize = 28

// Record is one journaled entry. Payload is opaque to the WAL; callers encode
// their command bytes into it.
type Record struct {
	Seq     uint64
	TsNanos int64
	Type    uint16
	Flags   uint16
	Payload []byte
}

// encodeRecordInto frames r into dst, growing dst only when its capacity is too
// small, and returns the framed slice. Reusing a caller-owned dst across calls
// makes the hot append path zero-alloc; the returned slice aliases dst.
func encodeRecordInto(dst []byte, r Record) []byte {
	total := headerSize + len(r.Payload)
	if cap(dst) < total {
		dst = make([]byte, total)
	}
	buf := dst[:total]
	binary.LittleEndian.PutUint64(buf[0:8], r.Seq)
	binary.LittleEndian.PutUint64(buf[8:16], uint64(r.TsNanos))
	binary.LittleEndian.PutUint16(buf[16:18], r.Type)
	binary.LittleEndian.PutUint16(buf[18:20], r.Flags)
	binary.LittleEndian.PutUint32(buf[20:24], uint32(len(r.Payload)))
	binary.LittleEndian.PutUint32(buf[24:28], crc32.ChecksumIEEE(r.Payload))
	copy(buf[headerSize:], r.Payload)
	return buf
}

// encodeRecord frames r into a freshly allocated buffer. Off-hot-path helper
// (tests); the hot append path uses the reusable encodeRecordInto.
func encodeRecord(r Record) []byte {
	return encodeRecordInto(nil, r)
}

// decodeRecord parses a record at buf[off:]. consumed is the number of bytes
// the record occupies. complete is false when the buffer is too short to hold
// the full record (a torn tail). crcOK reports whether the payload checksum
// matches.
func decodeRecord(buf []byte, off int) (r Record, consumed int, complete bool, crcOK bool) {
	if len(buf)-off < headerSize {
		return Record{}, 0, false, false
	}
	h := buf[off : off+headerSize]
	r.Seq = binary.LittleEndian.Uint64(h[0:8])
	r.TsNanos = int64(binary.LittleEndian.Uint64(h[8:16]))
	r.Type = binary.LittleEndian.Uint16(h[16:18])
	r.Flags = binary.LittleEndian.Uint16(h[18:20])
	plen := int(binary.LittleEndian.Uint32(h[20:24]))
	wantCRC := binary.LittleEndian.Uint32(h[24:28])
	if len(buf)-off-headerSize < plen {
		return Record{}, 0, false, false // payload truncated -> torn tail
	}
	payload := make([]byte, plen)
	copy(payload, buf[off+headerSize:off+headerSize+plen])
	r.Payload = payload
	return r, headerSize + plen, true, crc32.ChecksumIEEE(payload) == wantCRC
}
