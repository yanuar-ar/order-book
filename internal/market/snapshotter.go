package market

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// snapshotExt is the snapshot file extension. Files are named by zero-padded Seq
// watermark so lexical order equals numeric order.
const snapshotExt = ".snap"

// Snapshotter drives periodic Engine snapshots and bounds snapshot-file growth.
// It is single-threaded by design: the engine's run loop calls Maybe after each
// processed command (on the sequencer goroutine), so a snapshot is always taken
// at a quiesced command boundary and never races the writer.
//
// Triggers are independent: a count-based threshold, a wall-clock interval, or
// both. The wall-clock read does not affect deterministic state — a snapshot is
// a read-only capture that gets no Seq and is never journaled.
type Snapshotter struct {
	dir      string
	everyN   int64
	interval int64        // seconds; 0 disables the time trigger
	retainK  int          // snapshot files to keep
	now      func() int64 // unix seconds; injected for deterministic tests

	lastCount int64 // applied-command count at the last snapshot
	lastTime  int64 // wall-clock seconds at the last snapshot
	started   bool
}

// NewSnapshotter builds a snapshotter writing to dir. everyN and intervalSecs are
// the count and time triggers (0 disables each); retainK is the number of files
// to keep (clamped to >= 1). now supplies wall-clock seconds.
func NewSnapshotter(dir string, everyN, intervalSecs int64, retainK int, now func() int64) *Snapshotter {
	if retainK < 1 {
		retainK = 1
	}
	return &Snapshotter{dir: dir, everyN: everyN, interval: intervalSecs, retainK: retainK, now: now}
}

// due reports whether a snapshot should be taken given the current applied count.
func (s *Snapshotter) due(applied int64) bool {
	if !s.started {
		return false // first snapshot is taken explicitly via Snapshot/anchor
	}
	if s.everyN > 0 && applied-s.lastCount >= s.everyN {
		return true
	}
	if s.interval > 0 && s.now()-s.lastTime >= s.interval {
		return true
	}
	return false
}

// Anchor records the starting count/time without writing a snapshot. Call once
// after recovery so the first cadence window is measured from resume.
func (s *Snapshotter) Anchor(applied int64) {
	s.lastCount = applied
	s.lastTime = s.now()
	s.started = true
}

// Maybe takes a snapshot if a trigger has fired since the last one, returning
// whether it wrote. applied is the cumulative count of processed commands.
func (s *Snapshotter) Maybe(e *Engine, applied int64) (bool, error) {
	if !s.due(applied) {
		return false, nil
	}
	if err := s.Snapshot(e, applied); err != nil {
		return false, err
	}
	return true, nil
}

// Snapshot writes a snapshot now (used by Maybe and by graceful shutdown),
// names it by the engine's Seq watermark, resets the cadence window, and GCs old
// files. Publishing happens inside Engine.Snapshot, which forces WAL durability
// through the watermark before the atomic rename.
func (s *Snapshotter) Snapshot(e *Engine, applied int64) error {
	e.Drain()
	if err := e.Fatal(); err != nil {
		// A fail-stop during the drain means the WAL is broken; abort rather than
		// publish a snapshot that could capture state the log cannot back.
		return err
	}
	path := filepath.Join(s.dir, fmt.Sprintf("%020d%s", uint64(e.Seq()), snapshotExt))
	if err := e.Snapshot(path); err != nil {
		return err
	}
	s.lastCount = applied
	s.lastTime = s.now()
	s.started = true
	return s.gc()
}

// gc removes all but the newest retainK snapshot files. WAL segments are never
// touched — they are the full source of truth in v1.
func (s *Snapshotter) gc() error {
	files, err := snapshotFiles(s.dir)
	if err != nil {
		return err
	}
	if len(files) <= s.retainK {
		return nil
	}
	for _, f := range files[:len(files)-s.retainK] {
		if err := os.Remove(filepath.Join(s.dir, f)); err != nil {
			return err
		}
	}
	return nil
}

// snapshotFiles returns the snapshot file names in dir, ascending (oldest Seq
// first). The zero-padded names make lexical order match numeric order.
func snapshotFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == snapshotExt {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// LatestSnapshot returns the path to the highest-Seq snapshot file in dir and
// whether one exists. Recovery loads this on startup.
func LatestSnapshot(dir string) (string, bool) {
	files, err := snapshotFiles(dir)
	if err != nil || len(files) == 0 {
		return "", false
	}
	return filepath.Join(dir, files[len(files)-1]), true
}
