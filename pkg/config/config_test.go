package config

import "testing"

// envMap returns a getenv func backed by a map for deterministic tests.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaultsWhenUnset(t *testing.T) {
	c, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := Default()
	if c.RingSize != def.RingSize {
		t.Errorf("RingSize = %d, want %d", c.RingSize, def.RingSize)
	}
	if len(c.Markets) != len(def.Markets) {
		t.Errorf("Markets = %v, want %v", c.Markets, def.Markets)
	}
	if c.WALPath != def.WALPath {
		t.Errorf("WALPath = %q, want %q", c.WALPath, def.WALPath)
	}
}

func TestLoadOverridesFromEnv(t *testing.T) {
	c, err := Load(envMap(map[string]string{
		"OB_MARKETS":   "BTC/USDT, ETH/USDT",
		"OB_RING_SIZE": "1024",
		"OB_WAL_PATH":  "/tmp/wal",
		"OB_MAKER_FEE": "50000",
		"OB_TAKER_FEE": "100000",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c.Markets; len(got) != 2 || got[0] != "BTC/USDT" || got[1] != "ETH/USDT" {
		t.Errorf("Markets = %v, want [BTC/USDT ETH/USDT] (trimmed)", got)
	}
	if c.RingSize != 1024 {
		t.Errorf("RingSize = %d, want 1024", c.RingSize)
	}
	if c.WALPath != "/tmp/wal" {
		t.Errorf("WALPath = %q, want /tmp/wal", c.WALPath)
	}
	if c.MakerFee != 50000 || c.TakerFee != 100000 {
		t.Errorf("fees = (%d,%d), want (50000,100000)", c.MakerFee, c.TakerFee)
	}
}

func TestLoadRejectsNegativeFee(t *testing.T) {
	_, err := Load(envMap(map[string]string{"OB_TAKER_FEE": "-1"}))
	if err == nil {
		t.Fatal("expected error for negative taker fee, got nil")
	}
}

func TestLoadRejectsNonPowerOfTwoRing(t *testing.T) {
	_, err := Load(envMap(map[string]string{"OB_RING_SIZE": "1000"}))
	if err == nil {
		t.Fatal("expected error for non-power-of-two ring size, got nil")
	}
}

func TestLoadRejectsMalformedMarket(t *testing.T) {
	_, err := Load(envMap(map[string]string{"OB_MARKETS": "BTCUSDT"}))
	if err == nil {
		t.Fatal("expected error for market without '/', got nil")
	}
}

func TestLoadRejectsNonNumericInt(t *testing.T) {
	_, err := Load(envMap(map[string]string{"OB_PRICE_SCALE": "abc"}))
	if err == nil {
		t.Fatal("expected error for non-numeric price scale, got nil")
	}
}

// ---- Snapshot config ----

func TestLoadSnapshotDefaults(t *testing.T) {
	c, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.SnapshotPath != "./data/snapshots" {
		t.Errorf("SnapshotPath = %q, want ./data/snapshots", c.SnapshotPath)
	}
	if c.SnapshotIntervalSecs != 3600 {
		t.Errorf("SnapshotIntervalSecs = %d, want 3600", c.SnapshotIntervalSecs)
	}
	if c.SnapshotEveryN != 0 {
		t.Errorf("SnapshotEveryN = %d, want 0", c.SnapshotEveryN)
	}
	if c.SnapshotRetainK != 3 {
		t.Errorf("SnapshotRetainK = %d, want 3", c.SnapshotRetainK)
	}
}

func TestLoadSnapshotOverrides(t *testing.T) {
	c, err := Load(envMap(map[string]string{
		"OB_SNAPSHOT_PATH":     "/tmp/snaps",
		"OB_SNAPSHOT_EVERY":    "5000",
		"OB_SNAPSHOT_INTERVAL": "0",
		"OB_SNAPSHOT_RETAIN":   "10",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.SnapshotPath != "/tmp/snaps" || c.SnapshotEveryN != 5000 || c.SnapshotIntervalSecs != 0 || c.SnapshotRetainK != 10 {
		t.Errorf("snapshot config = %+v, want path=/tmp/snaps every=5000 interval=0 retain=10",
			[]any{c.SnapshotPath, c.SnapshotEveryN, c.SnapshotIntervalSecs, c.SnapshotRetainK})
	}
}

func TestLoadSnapshotTimeOnlyAndCountOnlyValid(t *testing.T) {
	// Count-only.
	if _, err := Load(envMap(map[string]string{"OB_SNAPSHOT_EVERY": "1000", "OB_SNAPSHOT_INTERVAL": "0"})); err != nil {
		t.Fatalf("count-only cadence rejected: %v", err)
	}
	// Time-only (the default interval already gives this, but be explicit).
	if _, err := Load(envMap(map[string]string{"OB_SNAPSHOT_EVERY": "0", "OB_SNAPSHOT_INTERVAL": "60"})); err != nil {
		t.Fatalf("time-only cadence rejected: %v", err)
	}
}

func TestLoadRejectsNoSnapshotCadence(t *testing.T) {
	if _, err := Load(envMap(map[string]string{"OB_SNAPSHOT_EVERY": "0", "OB_SNAPSHOT_INTERVAL": "0"})); err == nil {
		t.Fatal("expected error when both snapshot triggers are disabled")
	}
}

func TestLoadRejectsZeroRetain(t *testing.T) {
	if _, err := Load(envMap(map[string]string{"OB_SNAPSHOT_RETAIN": "0"})); err == nil {
		t.Fatal("expected error for OB_SNAPSHOT_RETAIN=0")
	}
}

func TestLoadRejectsSnapshotPathEqualWAL(t *testing.T) {
	if _, err := Load(envMap(map[string]string{"OB_SNAPSHOT_PATH": "./data/wal"})); err == nil {
		t.Fatal("expected error when snapshot path equals WAL path")
	}
}

// ---- Per-market filters ----

func TestLoadFilterDefaultsForDefaultMarkets(t *testing.T) {
	c, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, m := range c.Markets {
		f, ok := c.Filters[m]
		if !ok {
			t.Fatalf("market %q has no filter spec", m)
		}
		if f != defaultFilter() {
			t.Errorf("market %q filter = %+v, want default %+v", m, f, defaultFilter())
		}
	}
}

func TestLoadFilterEnvOverride(t *testing.T) {
	c, err := Load(envMap(map[string]string{
		"OB_FILTER_BTC_USDT_TICK_SIZE":    "500000",
		"OB_FILTER_BTC_USDT_MIN_NOTIONAL": "2000000000",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f := c.Filters["BTC/USDT"]
	if f.TickSize != 500_000 {
		t.Errorf("TickSize = %d, want 500000", f.TickSize)
	}
	if f.MinNotional != 2_000_000_000 {
		t.Errorf("MinNotional = %d, want 2000000000", f.MinNotional)
	}
	// Other default markets keep their defaults.
	if c.Filters["ETH/USDT"] != defaultFilter() {
		t.Errorf("ETH/USDT filter unexpectedly changed: %+v", c.Filters["ETH/USDT"])
	}
}

func TestLoadRejectsMarketWithoutFilters(t *testing.T) {
	// A market not among the defaults and with no filter env has a zero filter,
	// which Validate must reject (mandatory per-market filters).
	_, err := Load(envMap(map[string]string{"OB_MARKETS": "FOO/BAR"}))
	if err == nil {
		t.Fatal("expected error for market with no filter spec, got nil")
	}
}

func TestFilterValidateRejectsBadValues(t *testing.T) {
	base := defaultFilter()
	cases := []struct {
		name   string
		mutate func(*FilterSpec)
	}{
		{"zero tick", func(f *FilterSpec) { f.TickSize = 0 }},
		{"negative step", func(f *FilterSpec) { f.StepSize = -1 }},
		{"zero market step", func(f *FilterSpec) { f.MktStepSize = 0 }},
		{"inverted price bounds", func(f *FilterSpec) { f.MinPrice, f.MaxPrice = f.MaxPrice, f.MinPrice }},
		{"inverted lot bounds", func(f *FilterSpec) { f.MinQty, f.MaxQty = f.MaxQty, f.MinQty }},
		{"inverted notional bounds", func(f *FilterSpec) { f.MinNotional, f.MaxNotional = f.MaxNotional, f.MinNotional }},
		{"zero max notional", func(f *FilterSpec) { f.MaxNotional = 0 }},
		{"minPrice off-tick", func(f *FilterSpec) { f.MinPrice = f.TickSize + 1 }},
		{"minQty off-step", func(f *FilterSpec) { f.MinQty = f.StepSize + 1 }},
		{"mktMinQty off-step", func(f *FilterSpec) { f.MktMinQty = f.MktStepSize + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			tc.mutate(&f)
			c := Default()
			c.Filters["BTC/USDT"] = f
			if err := c.Validate(); err == nil {
				t.Fatalf("expected Validate error for %q, got nil", tc.name)
			}
		})
	}
}

func TestFilterValidateAcceptsWellFormed(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("default filters rejected: %v", err)
	}
}
