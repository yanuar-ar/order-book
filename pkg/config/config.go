// Package config loads spot-engine configuration from environment variables.
//
// It deliberately has no third-party dependencies and is never touched on the
// hot path; it is read once at startup to build the engine.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the resolved engine configuration. All fields are plain values so
// the struct can be copied freely and logged at startup.
type Config struct {
	Markets    []string // base/quote symbols, e.g. "BTC/USDT"
	RingSize   uint64   // SPSC ring capacity per link; power of two
	WALPath    string   // write-ahead log directory
	PriceScale int64    // fixed-point scale for prices
	QtyScale   int64    // fixed-point scale for quantities
	FeeScale   int64    // fixed-point scale for fee rates
	MakerFee   int64    // maker fee rate at FeeScale; must be >= 0
	TakerFee   int64    // taker fee rate at FeeScale; must be >= 0
}

// Default returns the built-in defaults used when no environment is set.
func Default() Config {
	return Config{
		Markets:    []string{"BTC/USDT", "ETH/USDT", "SOL/USDT"},
		RingSize:   1 << 16,
		WALPath:    "./data/wal",
		PriceScale: 100_000_000,
		QtyScale:   100_000_000,
		FeeScale:   100_000_000,
		MakerFee:   0,
		TakerFee:   0,
	}
}

// Load builds a Config from environment variables, falling back to Default for
// any unset key. It validates the result before returning.
//
// getenv is injected so tests can supply a deterministic environment; pass
// os.Getenv in production (see LoadFromOS).
func Load(getenv func(string) string) (Config, error) {
	c := Default()
	var err error

	if v := getenv("OB_MARKETS"); v != "" {
		c.Markets = splitMarkets(v)
	}
	if c.RingSize, err = envUint(getenv, "OB_RING_SIZE", c.RingSize); err != nil {
		return Config{}, err
	}
	if v := getenv("OB_WAL_PATH"); v != "" {
		c.WALPath = v
	}
	if c.PriceScale, err = envInt(getenv, "OB_PRICE_SCALE", c.PriceScale); err != nil {
		return Config{}, err
	}
	if c.QtyScale, err = envInt(getenv, "OB_QTY_SCALE", c.QtyScale); err != nil {
		return Config{}, err
	}
	if c.FeeScale, err = envInt(getenv, "OB_FEE_SCALE", c.FeeScale); err != nil {
		return Config{}, err
	}
	if c.MakerFee, err = envInt(getenv, "OB_MAKER_FEE", c.MakerFee); err != nil {
		return Config{}, err
	}
	if c.TakerFee, err = envInt(getenv, "OB_TAKER_FEE", c.TakerFee); err != nil {
		return Config{}, err
	}

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// LoadFromOS loads configuration from the process environment.
func LoadFromOS() (Config, error) { return Load(os.Getenv) }

// Validate checks invariants the engine relies on.
func (c Config) Validate() error {
	if len(c.Markets) == 0 {
		return fmt.Errorf("config: at least one market required")
	}
	for _, m := range c.Markets {
		if !strings.Contains(m, "/") {
			return fmt.Errorf("config: market %q must be base/quote", m)
		}
	}
	if c.RingSize == 0 || c.RingSize&(c.RingSize-1) != 0 {
		return fmt.Errorf("config: OB_RING_SIZE must be a power of two, got %d", c.RingSize)
	}
	if c.PriceScale <= 0 || c.QtyScale <= 0 || c.FeeScale <= 0 {
		return fmt.Errorf("config: scales must be positive (price=%d qty=%d fee=%d)", c.PriceScale, c.QtyScale, c.FeeScale)
	}
	if c.MakerFee < 0 || c.TakerFee < 0 {
		return fmt.Errorf("config: fees must be >= 0 (maker=%d taker=%d)", c.MakerFee, c.TakerFee)
	}
	return nil
}

func splitMarkets(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envInt(getenv func(string) string, key string, def int64) (int64, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer, got %q", key, v)
	}
	return n, nil
}

func envUint(getenv func(string) string, key string, def uint64) (uint64, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a non-negative integer, got %q", key, v)
	}
	return n, nil
}
