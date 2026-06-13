package wal

import (
	"fmt"
	"os"
	"path/filepath"
)

const defaultSegmentSize = 1 << 30 // 1 GiB

// Writer appends records to segmented log files in a directory. It is used by a
// single writer goroutine (the sequencer).
type Writer struct {
	dir     string
	segSize int64
	idx     int
	cur     *os.File
	curSize int64
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

// Append writes one record, rolling to a new segment when the current one would
// exceed the segment size. The bytes are not durable until Sync returns.
func (w *Writer) Append(r Record) error {
	enc := encodeRecord(r)
	if w.curSize > 0 && w.curSize+int64(len(enc)) > w.segSize {
		if err := w.roll(); err != nil {
			return err
		}
	}
	n, err := w.cur.Write(enc)
	w.curSize += int64(n)
	return err
}

func (w *Writer) roll() error {
	if err := w.cur.Sync(); err != nil {
		return err
	}
	if err := w.cur.Close(); err != nil {
		return err
	}
	w.idx++
	return w.openSegment()
}

// Sync flushes buffered writes to durable storage (group-commit point). The
// caller batches many Appends per Sync to amortize the fsync cost.
func (w *Writer) Sync() error { return w.cur.Sync() }

// Close syncs and closes the current segment.
func (w *Writer) Close() error {
	if w.cur == nil {
		return nil
	}
	err := w.cur.Sync()
	if cerr := w.cur.Close(); err == nil {
		err = cerr
	}
	w.cur = nil
	return err
}
