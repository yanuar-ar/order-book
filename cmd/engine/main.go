// Command engine wires and runs the in-process spot order-book engine.
//
// This is a stub: it loads configuration and logs it. Component wiring lands
// in U8 (market shard + engine assembly).
package main

import (
	"log/slog"

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
	log.Info("engine configured",
		slog.Any("markets", cfg.Markets),
		slog.Uint64("ring_size", cfg.RingSize),
		slog.String("wal_path", cfg.WALPath),
	)
	log.Info("engine wiring not yet implemented (lands in U8)")
}
