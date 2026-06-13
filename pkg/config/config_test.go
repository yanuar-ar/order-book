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
