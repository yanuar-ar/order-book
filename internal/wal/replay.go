package wal

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// ErrSeqGap is returned when replay encounters a missing sequence number — a
// real hole in the log. Recovery halts rather than guessing.
var ErrSeqGap = errors.New("wal: sequence gap in log")

// ErrCorrupt is returned when a fully-present record fails its CRC check
// somewhere other than the tail (genuine corruption, not a torn write).
var ErrCorrupt = errors.New("wal: corrupt record")

// Replay reads records with Seq > afterSeq from the segments in dir, in order,
// invoking fn for each. It enforces contiguity (the first applied record must
// be afterSeq+1, then each subsequent Seq increments by one) and halts with
// ErrSeqGap on a hole. A torn or partially-written record at the very tail is
// treated as not-yet-durable: replay stops cleanly there. A bad CRC before the
// tail is ErrCorrupt.
//
// When the log's maximum Seq is <= afterSeq (e.g., a snapshot ahead of the WAL
// tail), no records are applied and Replay returns nil — it does not halt.
func Replay(dir string, afterSeq uint64, fn func(Record) error) error {
	segs, err := filepath.Glob(filepath.Join(dir, "*.wal"))
	if err != nil {
		return err
	}
	sort.Strings(segs)

	next := afterSeq + 1
	started := false
	for si, seg := range segs {
		buf, err := os.ReadFile(seg)
		if err != nil {
			return err
		}
		lastSeg := si == len(segs)-1
		off := 0
		for off < len(buf) {
			rec, consumed, complete, crcOK := decodeRecord(buf, off)
			if !complete {
				// Truncated record. Only acceptable at the tail of the last segment.
				if lastSeg {
					return nil
				}
				return ErrCorrupt
			}
			atTail := lastSeg && off+consumed == len(buf)
			if !crcOK {
				if atTail {
					return nil // torn write at the tail
				}
				return ErrCorrupt
			}
			off += consumed

			if rec.Seq <= afterSeq {
				continue // already captured by the snapshot
			}
			if !started {
				if rec.Seq != next {
					return ErrSeqGap
				}
				started = true
			} else if rec.Seq != next {
				return ErrSeqGap
			}
			if err := fn(rec); err != nil {
				return err
			}
			next++
		}
	}
	return nil
}
