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
