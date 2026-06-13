package market

import (
	"reflect"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// ---- Positive ----

func TestOpenMapSnapshot_RoundTrip(t *testing.T) {
	open := map[types.OrderID]openOrder{
		10: {market: 0, account: 1, side: types.Buy, ordType: types.Limit, price: 100, qty: 5},
		11: {market: 2, account: 7, side: types.Sell, ordType: types.StopLimit, price: 90, qty: 3},
	}
	got, err := decodeOpenMap(encodeOpenMap(open))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(open, got) {
		t.Fatalf("open map differs:\n want %+v\n got  %+v", open, got)
	}
}

// ---- Edge: deterministic bytes regardless of map construction ----

func TestOpenMapSnapshot_DeterministicBytes(t *testing.T) {
	a := map[types.OrderID]openOrder{}
	b := map[types.OrderID]openOrder{}
	// Insert the same entries in opposite orders.
	ids := []types.OrderID{5, 1, 9, 3, 7}
	for _, id := range ids {
		a[id] = openOrder{market: 0, account: 1, side: types.Buy, ordType: types.Limit, price: types.Price(id), qty: 1}
	}
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		b[id] = openOrder{market: 0, account: 1, side: types.Buy, ordType: types.Limit, price: types.Price(id), qty: 1}
	}
	if string(encodeOpenMap(a)) != string(encodeOpenMap(b)) {
		t.Fatal("encodeOpenMap is not deterministic across map construction order")
	}
}

func TestOpenMapSnapshot_EmptyRoundTrips(t *testing.T) {
	got, err := decodeOpenMap(encodeOpenMap(map[types.OrderID]openOrder{}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty open map restored with %d entries", len(got))
	}
}

// ---- Negative ----

func TestOpenMapSnapshot_TruncatedRejected(t *testing.T) {
	full := encodeOpenMap(map[types.OrderID]openOrder{10: {market: 0, account: 1, side: types.Buy, ordType: types.Limit, price: 100, qty: 5}})
	for _, n := range []int{0, 2, len(full) - 1} {
		if _, err := decodeOpenMap(full[:n]); err == nil {
			t.Fatalf("decodeOpenMap accepted truncated section of len %d", n)
		}
	}
}
