package main

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

func eq(a [][]types.MarketID, b [][]types.MarketID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// ---- positive ----

func TestParseCoresIsolatedAndShared(t *testing.T) {
	got := parseCores("0;1,2")
	want := [][]types.MarketID{{0}, {1, 2}}
	if !eq(got, want) {
		t.Fatalf("parseCores(\"0;1,2\") = %v, want %v", got, want)
	}
}

func TestParseCoresAllIsolated(t *testing.T) {
	got := parseCores("0;1;2")
	want := [][]types.MarketID{{0}, {1}, {2}}
	if !eq(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseCoresSingle(t *testing.T) {
	if got := parseCores("0"); !eq(got, [][]types.MarketID{{0}}) {
		t.Fatalf("got %v, want [[0]]", got)
	}
}

// ---- edge ----

func TestParseCoresWhitespaceAndTrailingSep(t *testing.T) {
	got := parseCores("  0 ; 1 , 2 ;")
	want := [][]types.MarketID{{0}, {1, 2}}
	if !eq(got, want) {
		t.Fatalf("got %v, want %v (whitespace + trailing ';')", got, want)
	}
}

// ---- negative ----

func TestParseCoresEmpty(t *testing.T) {
	if got := parseCores(""); len(got) != 0 {
		t.Fatalf("empty input = %v, want no workers", got)
	}
}

func TestParseCoresInvalidTokensSkipped(t *testing.T) {
	// Non-numeric tokens are skipped; a group with only invalid tokens vanishes.
	got := parseCores("x;1")
	want := [][]types.MarketID{{1}}
	if !eq(got, want) {
		t.Fatalf("parseCores(\"x;1\") = %v, want %v", got, want)
	}
}
