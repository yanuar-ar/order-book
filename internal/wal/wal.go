package wal

import (
	"fmt"
	"os"
	"path/filepath"
)

const defaultSegmentSize = 1 << 30 // 1 GiB

// Writer appends records to segmented log files in a directory. It is used by a
// single writer goroutine (the sequencer).
//
// Append buffers framed records in memory; one write(2) per Sync flushes the
// whole batch to the OS, then Sync fsyncs it. Batching the write syscall (not
// just the fsync) is what lets group-commit amortize both syscalls over a batch
// — the durable hot path's dominant costs. Buffering until Sync is safe under
// the durable-ack barrier: nothing is durable (and no ack is released) until the
// watermark advances on Sync, so un-Synced pending records were never observable.
type Writer struct {
	dir     string
	segSize int64
	idx     int
	cur     *os.File
	curSize int64
	encBuf  []byte // reusable per-record framing buffer
	pending []byte // framed records buffered since the last flush (one write per Sync)
}

// OpenWriter creates (or reuses) the log directory and opens a fresh segment.
// segSize <= 0 selects the default segment size.
func OpenWriter(dir string, segSize int64) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if segSize <= 0 {
		segSize = defaultSegmentSize
	}
	w := &Writer{dir: dir, segSize: segSize, idx: nextSegmentIndex(dir)}
	if err := w.openSegment(); err != nil {
		return nil, err
	}
	return w, nil
}

func segmentName(dir string, idx int) string {
	return filepath.Join(dir, fmt.Sprintf("%06d.wal", idx))
}

func nextSegmentIndex(dir string) int {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.wal"))
	return len(matches)
}

func (w *Writer) openSegment() error {
	f, err := os.OpenFile(segmentName(w.dir, w.idx), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.cur = f
	w.curSize = info.Size()
	return nil
}

// Append frames r and buffers it in memory; the bytes reach the OS on the next
// Sync and are not durable until Sync returns. Rolling to a new segment, when
// the current one would overflow, flushes the pending batch first so a record
// never spans segments.
//
// Append copies r.Payload (into the pending buffer), so it does not retain
// r.Payload after returning — the caller may pass a reusable payload buffer.
// Single-writer only (the sequencer goroutine).
func (w *Writer) Append(r Record) error {
	w.encBuf = encodeRecordInto(w.encBuf, r)
	enc := w.encBuf
	// Segment occupancy includes both written bytes and the un-flushed batch.
	occupied := w.curSize + int64(len(w.pending))
	if occupied > 0 && occupied+int64(len(enc)) > w.segSize {
		if err := w.roll(); err != nil { // flushes pending, syncs, opens a fresh segment
			return err
		}
	}
	w.pending = append(w.pending, enc...)
	return nil
}

// flushPending writes the buffered batch to the current segment in one write and
// resets the buffer (keeping its capacity). It does not fsync.
func (w *Writer) flushPending() error {
	if len(w.pending) == 0 {
		return nil
	}
	n, err := w.cur.Write(w.pending)
	w.curSize += int64(n)
	w.pending = w.pending[:0]
	return err
}

func (w *Writer) roll() error {
	if err := w.flushPending(); err != nil {
		return err
	}
	if err := w.cur.Sync(); err != nil {
		return err
	}
	if err := w.cur.Close(); err != nil {
		return err
	}
	w.idx++
	return w.openSegment()
}

// Sync flushes the buffered batch with one write and fsyncs it — the
// group-commit point. The caller batches many Appends per Sync to amortize both
// syscalls.
func (w *Writer) Sync() error {
	if err := w.flushPending(); err != nil {
		return err
	}
	return w.cur.Sync()
}

// Close flushes, syncs, and closes the current segment.
func (w *Writer) Close() error {
	if w.cur == nil {
		return nil
	}
	err := w.flushPending()
	if serr := w.cur.Sync(); err == nil {
		err = serr
	}
	if cerr := w.cur.Close(); err == nil {
		err = cerr
	}
	w.cur = nil
	return err
}
