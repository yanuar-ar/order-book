package types

import "testing"

func TestCommandEncodeDecodeRoundTrip(t *testing.T) {
	c := Command{
		Seq: 7, TsNanos: 123456, Type: CmdNewOrder, Market: 2, Account: 99,
		OrderID: 555, Side: Sell, OrdType: StopLimit, Tif: IOC, Flags: FlagIceberg,
		Price: 100_000_000, StopPrice: 95_000_000, Qty: 5_000, DisplayQty: 1_000,
		Asset: 3, Amount: 42, ClientReqID: 88, ClientTsNanos: 777,
	}
	got, err := DecodeCommand(EncodeCommand(c))
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if got != c {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, c)
	}
}

func TestDecodeCommandRejectsShortBuffer(t *testing.T) {
	if _, err := DecodeCommand([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error decoding a truncated command buffer")
	}
}
