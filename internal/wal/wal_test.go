package wal

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// payload encodes seq into a fixed 4-byte payload so every record is exactly
// headerSize+4 = 32 bytes, making byte offsets in corruption tests predictable.
func payload(seq uint64) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(seq))
	return b
}

func writeSeqs(t *testing.T, dir string, segSize int64, seqs ...uint64) {
	t.Helper()
	w, err := OpenWriter(dir, segSize)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	for _, s := range seqs {
		if err := w.Append(Record{Seq: s, Payload: payload(s)}); err != nil {
			t.Fatalf("Append(%d): %v", s, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func collect(t *testing.T, dir string, after uint64) ([]Record, error) {
	t.Helper()
	var recs []Record
	err := Replay(dir, after, func(r Record) error {
		recs = append(recs, r)
		return nil
	})
	return recs, err
}

// ---- Positive ----

func TestAppendReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeSeqs(t, dir, 0, 1, 2, 3)
	recs, err := collect(t, dir, 0)
	if err != nil {
		t.Fatalf("replay error: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("replayed %d records, want 3", len(recs))
	}
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Errorf("record %d Seq = %d, want %d", i, r.Seq, i+1)
		}
		if binary.LittleEndian.Uint32(r.Payload) != uint32(i+1) {
			t.Errorf("record %d payload mismatch", i)
		}
	}
}

func TestSegmentRolloverPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	// Each record is 32 bytes; segSize 40 forces a new segment per record.
	writeSeqs(t, dir, 40, 1, 2, 3, 4)
	segs, _ := filepath.Glob(filepath.Join(dir, "*.wal"))
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(segs))
	}
	recs, err := collect(t, dir, 0)
	if err != nil || len(recs) != 4 {
		t.Fatalf("replay across segments = %d recs, err %v; want 4", len(recs), err)
	}
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("order broken across segments at %d: Seq %d", i, r.Seq)
		}
	}
}

func TestReplayFromWatermarkSkipsApplied(t *testing.T) {
	dir := t.TempDir()
	writeSeqs(t, dir, 0, 1, 2, 3, 4, 5)
	recs, err := collect(t, dir, 2) // snapshot already captured 1..2
	if err != nil {
		t.Fatalf("replay error: %v", err)
	}
	if len(recs) != 3 || recs[0].Seq != 3 || recs[2].Seq != 5 {
		t.Fatalf("watermark replay = %+v, want seqs 3,4,5", seqsOf(recs))
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap")
	sections := [][]byte{[]byte("book0-state"), []byte("ledger-state")}
	if err := WriteSnapshot(path, 42, 9, sections); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	seq, epoch, got, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if seq != 42 || epoch != 9 || len(got) != 2 || string(got[0]) != "book0-state" || string(got[1]) != "ledger-state" {
		t.Fatalf("snapshot round-trip mismatch: seq %d epoch %d sections %q", seq, epoch, got)
	}
}

// ---- Negative ----

func TestSeqGapHalts(t *testing.T) {
	dir := t.TempDir()
	writeSeqs(t, dir, 0, 1, 2, 4) // missing 3
	_, err := collect(t, dir, 0)
	if err != ErrSeqGap {
		t.Fatalf("replay error = %v, want ErrSeqGap", err)
	}
}

func TestCorruptMidStreamErrors(t *testing.T) {
	dir := t.TempDir()
	writeSeqs(t, dir, 0, 1, 2, 3)
	seg := filepath.Join(dir, "000000.wal")
	buf, _ := os.ReadFile(seg)
	buf[headerSize] ^= 0xFF // corrupt record 1's payload (not the tail)
	os.WriteFile(seg, buf, 0o644)
	_, err := collect(t, dir, 0)
	if err != ErrCorrupt {
		t.Fatalf("replay error = %v, want ErrCorrupt", err)
	}
}

func TestBadSnapshotCRC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap")
	WriteSnapshot(path, 7, 0, [][]byte{[]byte("x")})
	buf, _ := os.ReadFile(path)
	buf[4] ^= 0xFF // corrupt the seq field; CRC must catch it
	os.WriteFile(path, buf, 0o644)
	if _, _, _, err := ReadSnapshot(path); err != ErrBadSnapshot {
		t.Fatalf("ReadSnapshot err = %v, want ErrBadSnapshot", err)
	}
}

// ---- Edge ----

func TestTornTailTruncatedStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	writeSeqs(t, dir, 0, 1, 2, 3)
	seg := filepath.Join(dir, "000000.wal")
	buf, _ := os.ReadFile(seg)
	os.WriteFile(seg, buf[:len(buf)-2], 0o644) // cut into record 3
	recs, err := collect(t, dir, 0)
	if err != nil {
		t.Fatalf("torn tail should not error, got %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("torn tail replay = %d recs, want 2", len(recs))
	}
}

func TestBadCRCAtTailTreatedAsTorn(t *testing.T) {
	dir := t.TempDir()
	writeSeqs(t, dir, 0, 1, 2)
	seg := filepath.Join(dir, "000000.wal")
	buf, _ := os.ReadFile(seg)
	buf[len(buf)-1] ^= 0xFF // corrupt last record's payload (the tail)
	os.WriteFile(seg, buf, 0o644)
	recs, err := collect(t, dir, 0)
	if err != nil {
		t.Fatalf("bad CRC at tail should be torn (nil err), got %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("tail-torn replay = %d recs, want 1", len(recs))
	}
}

func TestSnapshotAheadOfWALDoesNotHalt(t *testing.T) {
	dir := t.TempDir()
	writeSeqs(t, dir, 0, 1, 2, 3)
	recs, err := collect(t, dir, 10) // snapshot ahead of the WAL tail
	if err != nil {
		t.Fatalf("snapshot-ahead replay errored: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("snapshot-ahead replay applied %d recs, want 0", len(recs))
	}
}

func TestEmptyLogReplaysNothing(t *testing.T) {
	dir := t.TempDir()
	w, _ := OpenWriter(dir, 0)
	w.Close()
	recs, err := collect(t, dir, 0)
	if err != nil || len(recs) != 0 {
		t.Fatalf("empty log replay = %d recs, err %v; want 0, nil", len(recs), err)
	}
}

func seqsOf(recs []Record) []uint64 {
	out := make([]uint64, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}
