// Command engine wires and runs the in-process spot order-book engine.
//
// v1 builds the engine from configuration and reports readiness. There is no
// network gateway; commands are submitted in-process (see internal/market and
// the bench harness).
package main

import (
	"log/slog"
	"strings"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
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
	eng := market.NewEngine(market.Config{
		Markets:  specs,
		QtyScale: cfg.QtyScale,
		FeeScale: cfg.FeeScale,
		MakerFee: cfg.MakerFee,
		TakerFee: cfg.TakerFee,
		RingSize: cfg.RingSize,
	})
	_ = eng

	log.Info("engine ready",
		slog.Int("markets", len(specs)),
		slog.Int("assets", len(assets)),
		slog.Uint64("ring_size", cfg.RingSize),
	)
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
