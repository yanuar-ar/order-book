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

	// Snapshot durability. Snapshots are a restart-speed optimization layered on
	// the full WAL; the WAL itself is never truncated in v1.
	SnapshotPath         string // snapshot directory; distinct from WALPath
	SnapshotEveryN       int64  // snapshot every N applied commands; 0 disables count-based
	SnapshotIntervalSecs int64  // snapshot every N seconds; 0 disables time-based
	SnapshotRetainK      int64  // keep the last K snapshot files; must be >= 1

	// Filters holds the per-market order-validation filters, keyed by the market
	// symbol (the same strings as Markets). Every market MUST have an entry; the
	// engine refuses to start otherwise. Values are integer fixed-point at the
	// configured scales (prices at PriceScale, quantities at QtyScale, notional
	// at PriceScale, matching types.Notional).
	Filters map[string]FilterSpec
}

// FilterSpec is the static, per-market set of CEX-style order filters enforced
// at submit time. Price and quantity values are fixed-point at PriceScale and
// QtyScale; notional bounds are quote-currency values at PriceScale (as produced
// by types.Notional). All values are validated as positive and internally
// consistent at startup; see Config.Validate.
type FilterSpec struct {
	// Price filter (limit and stop trigger/limit prices).
	TickSize int64 // price increment; price must be a multiple of TickSize
	MinPrice int64 // minimum price (inclusive)
	MaxPrice int64 // maximum price (inclusive)

	// Lot filter (limit and stop-limit quantities, plus iceberg total/display).
	StepSize int64 // quantity increment; qty must be a multiple of StepSize
	MinQty   int64 // minimum quantity (inclusive)
	MaxQty   int64 // maximum quantity (inclusive)

	// Market lot filter (market and stop-market quantities). Separate from the
	// limit lot filter, matching a CEX MARKET_LOT_SIZE.
	MktStepSize int64 // market-order quantity increment
	MktMinQty   int64 // minimum market-order quantity (inclusive)
	MktMaxQty   int64 // maximum market-order quantity (inclusive)

	// Notional filter (quote value = Notional(price, qty)). For market and
	// stop-market orders the reference price is the market's last-trade price;
	// when no trade has occurred the notional check is skipped (fail-open).
	MinNotional int64 // minimum notional (inclusive)
	MaxNotional int64 // maximum notional (inclusive)
}

// Default returns the built-in defaults used when no environment is set.
func Default() Config {
	markets := []string{"BTC/USDT", "ETH/USDT", "SOL/USDT"}
	filters := make(map[string]FilterSpec, len(markets))
	for _, m := range markets {
		filters[m] = defaultFilter()
	}
	return Config{
		Markets:    markets,
		RingSize:   1 << 16,
		WALPath:    "./data/wal",
		PriceScale: 100_000_000,
		QtyScale:   100_000_000,
		FeeScale:   100_000_000,
		MakerFee:   0,
		TakerFee:   0,

		SnapshotPath:         "./data/snapshots",
		SnapshotEveryN:       0,    // time-based by default
		SnapshotIntervalSecs: 3600, // every hour
		SnapshotRetainK:      3,

		Filters: filters,
	}
}

// defaultFilter returns a permissive but well-formed filter at the default
// 1e8 price/qty scale: 0.01 tick, 0.0001 lot, 10.00 min notional.
func defaultFilter() FilterSpec {
	return FilterSpec{
		TickSize:    1_000_000,             // 0.01
		MinPrice:    1_000_000,             // 0.01
		MaxPrice:    100_000_000_000_000,   // 1,000,000.00
		StepSize:    10_000,                // 0.0001
		MinQty:      10_000,                // 0.0001
		MaxQty:      1_000_000_000_000,     // 10,000.0000
		MktStepSize: 10_000,                // 0.0001
		MktMinQty:   10_000,                // 0.0001
		MktMaxQty:   1_000_000_000_000,     // 10,000.0000
		MinNotional: 1_000_000_000,         // 10.00
		MaxNotional: 1_000_000_000_000_000, // 10,000,000.00
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
	if v := getenv("OB_SNAPSHOT_PATH"); v != "" {
		c.SnapshotPath = v
	}
	if c.SnapshotEveryN, err = envInt(getenv, "OB_SNAPSHOT_EVERY", c.SnapshotEveryN); err != nil {
		return Config{}, err
	}
	if c.SnapshotIntervalSecs, err = envInt(getenv, "OB_SNAPSHOT_INTERVAL", c.SnapshotIntervalSecs); err != nil {
		return Config{}, err
	}
	if c.SnapshotRetainK, err = envInt(getenv, "OB_SNAPSHOT_RETAIN", c.SnapshotRetainK); err != nil {
		return Config{}, err
	}

	// Per-market filters. Each field falls back to the default-market filter when
	// present, so the default markets work with no extra env; a market introduced
	// via OB_MARKETS must supply its own filter values or Validate rejects it.
	filters := make(map[string]FilterSpec, len(c.Markets))
	for _, m := range c.Markets {
		f := c.Filters[m] // zero FilterSpec when m is not a default market
		if f, err = loadFilter(getenv, m, f); err != nil {
			return Config{}, err
		}
		filters[m] = f
	}
	c.Filters = filters

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// loadFilter resolves one market's filter from env keys of the form
// OB_FILTER_<SYMBOL>_<FIELD> (symbol uppercased with "/" and "-" mapped to "_"),
// each falling back to the corresponding field of def.
func loadFilter(getenv func(string) string, market string, def FilterSpec) (FilterSpec, error) {
	field := func(name string, d int64) (int64, error) {
		return envInt(getenv, filterEnvKey(market, name), d)
	}
	f := def
	var err error
	if f.TickSize, err = field("TICK_SIZE", def.TickSize); err != nil {
		return f, err
	}
	if f.MinPrice, err = field("MIN_PRICE", def.MinPrice); err != nil {
		return f, err
	}
	if f.MaxPrice, err = field("MAX_PRICE", def.MaxPrice); err != nil {
		return f, err
	}
	if f.StepSize, err = field("STEP_SIZE", def.StepSize); err != nil {
		return f, err
	}
	if f.MinQty, err = field("MIN_QTY", def.MinQty); err != nil {
		return f, err
	}
	if f.MaxQty, err = field("MAX_QTY", def.MaxQty); err != nil {
		return f, err
	}
	if f.MktStepSize, err = field("MKT_STEP_SIZE", def.MktStepSize); err != nil {
		return f, err
	}
	if f.MktMinQty, err = field("MKT_MIN_QTY", def.MktMinQty); err != nil {
		return f, err
	}
	if f.MktMaxQty, err = field("MKT_MAX_QTY", def.MktMaxQty); err != nil {
		return f, err
	}
	if f.MinNotional, err = field("MIN_NOTIONAL", def.MinNotional); err != nil {
		return f, err
	}
	if f.MaxNotional, err = field("MAX_NOTIONAL", def.MaxNotional); err != nil {
		return f, err
	}
	return f, nil
}

func filterEnvKey(market, field string) string {
	sym := strings.NewReplacer("/", "_", "-", "_").Replace(strings.ToUpper(market))
	return "OB_FILTER_" + sym + "_" + field
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
	if c.SnapshotPath == "" {
		return fmt.Errorf("config: OB_SNAPSHOT_PATH must be non-empty")
	}
	if c.SnapshotPath == c.WALPath {
		return fmt.Errorf("config: OB_SNAPSHOT_PATH must differ from OB_WAL_PATH (%q)", c.WALPath)
	}
	if c.SnapshotEveryN < 0 || c.SnapshotIntervalSecs < 0 {
		return fmt.Errorf("config: snapshot cadence must be >= 0 (every=%d interval=%d)", c.SnapshotEveryN, c.SnapshotIntervalSecs)
	}
	if c.SnapshotEveryN == 0 && c.SnapshotIntervalSecs == 0 {
		return fmt.Errorf("config: at least one of OB_SNAPSHOT_EVERY / OB_SNAPSHOT_INTERVAL must be > 0")
	}
	if c.SnapshotRetainK < 1 {
		return fmt.Errorf("config: OB_SNAPSHOT_RETAIN must be >= 1, got %d", c.SnapshotRetainK)
	}
	for _, m := range c.Markets {
		f, ok := c.Filters[m]
		if !ok {
			return fmt.Errorf("config: market %q missing filter spec", m)
		}
		if err := f.validate(m); err != nil {
			return err
		}
	}
	return nil
}

// validate checks that a filter is well-formed: positive increments and bounds,
// ordered ranges, and minimums aligned to their increment.
func (f FilterSpec) validate(market string) error {
	if f.TickSize <= 0 || f.StepSize <= 0 || f.MktStepSize <= 0 {
		return fmt.Errorf("config: market %q filter increments must be positive (tick=%d step=%d mktStep=%d)", market, f.TickSize, f.StepSize, f.MktStepSize)
	}
	if f.MinPrice <= 0 || f.MinPrice > f.MaxPrice {
		return fmt.Errorf("config: market %q price bounds invalid (min=%d max=%d)", market, f.MinPrice, f.MaxPrice)
	}
	if f.MinQty <= 0 || f.MinQty > f.MaxQty {
		return fmt.Errorf("config: market %q lot bounds invalid (min=%d max=%d)", market, f.MinQty, f.MaxQty)
	}
	if f.MktMinQty <= 0 || f.MktMinQty > f.MktMaxQty {
		return fmt.Errorf("config: market %q market-lot bounds invalid (min=%d max=%d)", market, f.MktMinQty, f.MktMaxQty)
	}
	if f.MinNotional < 0 || f.MaxNotional <= 0 || f.MinNotional > f.MaxNotional {
		return fmt.Errorf("config: market %q notional bounds invalid (min=%d max=%d)", market, f.MinNotional, f.MaxNotional)
	}
	if f.MinPrice%f.TickSize != 0 {
		return fmt.Errorf("config: market %q MinPrice %d not aligned to TickSize %d", market, f.MinPrice, f.TickSize)
	}
	if f.MinQty%f.StepSize != 0 {
		return fmt.Errorf("config: market %q MinQty %d not aligned to StepSize %d", market, f.MinQty, f.StepSize)
	}
	if f.MktMinQty%f.MktStepSize != 0 {
		return fmt.Errorf("config: market %q MktMinQty %d not aligned to MktStepSize %d", market, f.MktMinQty, f.MktStepSize)
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
