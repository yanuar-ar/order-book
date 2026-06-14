package wal

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
)

// snapshotMagic identifies a snapshot file.
const snapshotMagic uint32 = 0x534E4150 // "SNAP"

// ErrBadSnapshot is returned when a snapshot file is malformed or fails its CRC.
var ErrBadSnapshot = errors.New("wal: bad snapshot")

// WriteSnapshot writes a snapshot atomically: it serializes a watermark Seq and
// an ordered list of component sections (e.g., each book's and the ledger's
// serialized state), then renames into place so a crash never leaves a partial
// snapshot.
//
// Durability invariant (enforced by the caller): publish a snapshot at Seq=S
// only after the WAL is durably committed through S, so recovery never finds a
// snapshot ahead of the WAL tail.
func WriteSnapshot(path string, seq, epoch uint64, sections [][]byte) error {
	body := encodeSnapshot(seq, epoch, sections)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadSnapshot loads a snapshot file written by WriteSnapshot.
func ReadSnapshot(path string) (seq, epoch uint64, sections [][]byte, err error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, nil, err
	}
	return decodeSnapshot(buf)
}

func encodeSnapshot(seq, epoch uint64, sections [][]byte) []byte {
	// Layout: magic(4) seq(8) epoch(8) nSections(4) [len(4) bytes]... crc(4).
	size := 24
	for _, s := range sections {
		size += 4 + len(s)
	}
	buf := make([]byte, 0, size+4)
	var hdr [24]byte
	binary.LittleEndian.PutUint32(hdr[0:4], snapshotMagic)
	binary.LittleEndian.PutUint64(hdr[4:12], seq)
	binary.LittleEndian.PutUint64(hdr[12:20], epoch)
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(len(sections)))
	buf = append(buf, hdr[:]...)
	var lenbuf [4]byte
	for _, s := range sections {
		binary.LittleEndian.PutUint32(lenbuf[:], uint32(len(s)))
		buf = append(buf, lenbuf[:]...)
		buf = append(buf, s...)
	}
	var crcbuf [4]byte
	binary.LittleEndian.PutUint32(crcbuf[:], crc32.ChecksumIEEE(buf))
	return append(buf, crcbuf[:]...)
}

func decodeSnapshot(buf []byte) (uint64, uint64, [][]byte, error) {
	if len(buf) < 28 {
		return 0, 0, nil, ErrBadSnapshot
	}
	body := buf[:len(buf)-4]
	wantCRC := binary.LittleEndian.Uint32(buf[len(buf)-4:])
	if crc32.ChecksumIEEE(body) != wantCRC {
		return 0, 0, nil, ErrBadSnapshot
	}
	if binary.LittleEndian.Uint32(body[0:4]) != snapshotMagic {
		return 0, 0, nil, ErrBadSnapshot
	}
	seq := binary.LittleEndian.Uint64(body[4:12])
	epoch := binary.LittleEndian.Uint64(body[12:20])
	n := int(binary.LittleEndian.Uint32(body[20:24]))
	off := 24
	sections := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		if off+4 > len(body) {
			return 0, 0, nil, ErrBadSnapshot
		}
		l := int(binary.LittleEndian.Uint32(body[off : off+4]))
		off += 4
		if off+l > len(body) {
			return 0, 0, nil, ErrBadSnapshot
		}
		sec := make([]byte, l)
		copy(sec, body[off:off+l])
		sections = append(sections, sec)
		off += l
	}
	return seq, epoch, sections, nil
}
