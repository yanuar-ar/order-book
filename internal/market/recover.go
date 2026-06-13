package market

import (
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// Recover rebuilds an engine on startup. It loads the latest snapshot in snapDir
// and replays the WAL tail from walDir; if the snapshot is missing, corrupt
// (bad CRC), incompatible (config/version mismatch), or logically inconsistent
// (fails the post-rebuild self-check), it logs the reason and falls back to a
// full replay from Seq 0 — the WAL is the complete source of truth in v1, so the
// fallback is always reachable.
//
// After replaying, it primes the sequencer to the final journaled Seq so live
// commands continue contiguously, and re-enables stop activations (replay ran
// with stops suppressed because activations are already in the log).
//
// logf receives a one-line message whenever a snapshot is skipped, so a fallback
// is never silent. A nil logf discards the message.
func Recover(cfg Config, walDir, snapDir string, logf func(string, ...any)) (*Engine, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cfg.SuppressStops = true

	var e *Engine
	var afterSeq uint64
	if path, ok := LatestSnapshot(snapDir); ok {
		if re, err := Restore(cfg, path); err != nil {
			logf("recovery: snapshot %s unusable (%v) — falling back to full WAL replay from Seq 0", path, err)
		} else {
			e, afterSeq = re, uint64(re.Seq())
		}
	}
	if e == nil {
		e = NewEngine(cfg)
		afterSeq = 0
	}

	maxSeq := afterSeq
	err := wal.Replay(walDir, afterSeq, func(rec wal.Record) error {
		cmd, derr := types.DecodeCommand(rec.Payload)
		if derr != nil {
			return derr
		}
		e.ApplyJournaled(cmd)
		maxSeq = rec.Seq
		return nil
	})
	if err != nil {
		return nil, err
	}

	e.SetSeq(types.Seq(maxSeq))
	e.EnableStops()
	return e, nil
}
