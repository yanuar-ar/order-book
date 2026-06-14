// Command engine wires and runs the in-process spot order-book engine.
//
// v1 recovers state on startup (latest snapshot + WAL tail, or full replay),
// then runs a single sequencer loop with periodic snapshots until a shutdown
// signal, at which point it takes a final snapshot. There is no network gateway;
// commands are submitted in-process (see internal/market and the bench harness).
package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
	"github.com/yanuar-ar/order-book/pkg/config"
	"github.com/yanuar-ar/order-book/pkg/logger"
)

func main() {
	log := logger.Default()
	cfg, err := config.LoadFromOS()
	if err != nil {
		log.Error("config load failed", slog.Any("err", err))
		return
	}

	specs, assets := buildMarketSpecs(cfg.Markets)

	walW, err := wal.OpenWriter(cfg.WALPath, 0)
	if err != nil {
		log.Error("open WAL failed", slog.Any("err", err))
		return
	}

	mcfg := market.Config{
		Markets:         specs,
		Filters:         buildMarketFilters(cfg),
		QtyScale:        cfg.QtyScale,
		FeeScale:        cfg.FeeScale,
		MakerFee:        cfg.MakerFee,
		TakerFee:        cfg.TakerFee,
		RingSize:        cfg.RingSize,
		Journal:         walW,
		AsyncJournal:    cfg.JournalMode == "async",
		JournalRing:     cfg.JournalRing,
		ReplicationMode: cfg.ReplicationMode,
		ReplicationRing: cfg.ReplicationRing,
		WALDir:          cfg.WALPath, // so the standby can backfill overflow gaps from the WAL
	}
	eng, err := market.Recover(mcfg, cfg.WALPath, cfg.SnapshotPath, func(format string, args ...any) {
		log.Warn("recovery fallback", slog.String("detail", fmt.Sprintf(format, args...)))
	})
	if err != nil {
		log.Error("recovery failed", slog.Any("err", err))
		_ = walW.Close()
		return
	}

	if err := os.MkdirAll(cfg.SnapshotPath, 0o755); err != nil {
		log.Error("create snapshot dir failed", slog.Any("err", err))
		_ = walW.Close()
		return
	}
	snap := market.NewSnapshotter(cfg.SnapshotPath, cfg.SnapshotEveryN, cfg.SnapshotIntervalSecs,
		int(cfg.SnapshotRetainK), func() int64 { return time.Now().Unix() })
	snap.Anchor(int64(eng.Seq()))

	log.Info("engine ready",
		slog.Int("markets", len(specs)),
		slog.Int("assets", len(assets)),
		slog.Uint64("ring_size", cfg.RingSize),
		slog.Uint64("recovered_seq", uint64(eng.Seq())),
	)

	// Single sequencer loop: process commands and check snapshot cadence on one
	// goroutine, so a snapshot is always taken at a quiesced boundary and never
	// races the writer.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		replicatorReported := false
		for {
			select {
			case <-stop:
				return
			default:
				worked := eng.Step()
				if err := eng.Fatal(); err != nil {
					// WAL durability failed: the log is the source of truth, so we
					// must not match or release further output. Halt the loop and
					// fall through to graceful shutdown.
					log.Error("engine fail-stop: WAL durability failure", slog.Any("err", err))
					return
				}
				if err := eng.ReplicatorFatal(); err != nil && !replicatorReported {
					// The standby link died: the primary stays up (the WAL is intact).
					// In sync mode acks are now stalling; an operator must degrade-to-
					// solo to resume releasing them. Log once, do not stop.
					log.Error("replication fail: standby link down — degrade-to-solo to resume acks", slog.Any("err", err))
					replicatorReported = true
				}
				if _, err := snap.Maybe(eng, int64(eng.Seq())); err != nil {
					log.Error("snapshot failed", slog.Any("err", err))
				}
				if !worked {
					time.Sleep(time.Millisecond) // idle: no gateway feeds commands in v1
				}
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	close(stop)
	<-done // loop stopped: the engine is now exclusively ours for shutdown

	if err := snap.Snapshot(eng, int64(eng.Seq())); err != nil {
		log.Error("final snapshot failed", slog.Any("err", err))
	}
	// Stop the async journaller goroutine (if any) before closing the WAL it
	// writes to; a no-op for the sync journaller.
	if err := eng.Close(); err != nil {
		log.Error("journaller close failed", slog.Any("err", err))
	}
	if err := walW.Close(); err != nil {
		log.Error("WAL close failed", slog.Any("err", err))
	}
	log.Info("engine stopped", slog.Uint64("seq", uint64(eng.Seq())))
}

// buildMarketSpecs assigns a stable AssetID to each distinct asset symbol and
// maps each market (in config order) to its base/quote asset IDs.
func buildMarketSpecs(markets []string) (map[types.MarketID]balance.MarketSpec, map[string]types.AssetID) {
	assets := make(map[string]types.AssetID)
	assetID := func(sym string) types.AssetID {
		if id, ok := assets[sym]; ok {
			return id
		}
		id := types.AssetID(len(assets) + 1)
		assets[sym] = id
		return id
	}
	specs := make(map[types.MarketID]balance.MarketSpec, len(markets))
	for i, m := range markets {
		parts := strings.SplitN(m, "/", 2)
		if len(parts) != 2 {
			continue
		}
		specs[types.MarketID(i)] = balance.MarketSpec{Base: assetID(parts[0]), Quote: assetID(parts[1])}
	}
	return specs, assets
}

// buildMarketFilters maps each market (in config order, matching buildMarketSpecs)
// to its engine-internal filter set. Config.Validate has already guaranteed every
// market has a complete, consistent filter entry.
func buildMarketFilters(cfg config.Config) map[types.MarketID]types.MarketFilters {
	out := make(map[types.MarketID]types.MarketFilters, len(cfg.Markets))
	for i, m := range cfg.Markets {
		f := cfg.Filters[m]
		out[types.MarketID(i)] = types.MarketFilters{
			TickSize:    f.TickSize,
			MinPrice:    f.MinPrice,
			MaxPrice:    f.MaxPrice,
			StepSize:    f.StepSize,
			MinQty:      f.MinQty,
			MaxQty:      f.MaxQty,
			MktStepSize: f.MktStepSize,
			MktMinQty:   f.MktMinQty,
			MktMaxQty:   f.MktMaxQty,
			MinNotional: f.MinNotional,
			MaxNotional: f.MaxNotional,
		}
	}
	return out
}
