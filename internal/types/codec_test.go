package types

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// reflectiveEncode is the original reflective encoder, pinned here as the
// byte-layout reference. The hand-rolled EncodeCommand must match it exactly so
// existing WALs replay unchanged (durability contract).
func reflectiveEncode(c Command) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, &c)
	return b.Bytes()
}

// sampleCommands spans every CmdType plus a boundary-value command.
func sampleCommands() []Command {
	return []Command{
		{Seq: 7, TsNanos: 123456, Type: CmdNewOrder, Market: 2, Account: 99, OrderID: 555, Side: Sell, OrdType: StopLimit, Tif: IOC, Flags: FlagIceberg, Price: 100_000_000, StopPrice: 95_000_000, Qty: 5_000, DisplayQty: 1_000, Asset: 3, Amount: 42, ClientReqID: 88, ClientTsNanos: 777},
		{Type: CmdCancel, Market: 1, Account: 5, OrderID: 1234},
		{Type: CmdAmend, Market: 0, Account: 7, OrderID: 9, Price: 50, Qty: 3},
		{Type: CmdDeposit, Account: 11, Asset: 2, Amount: 1_000_000},
		{Type: CmdWithdraw, Account: 11, Asset: 2, Amount: 500},
		// Boundary values: max-width fields and every enum at its max.
		{Seq: ^Seq(0), TsNanos: 1<<62 - 1, Type: CmdWithdraw, Market: ^MarketID(0), Account: ^AccountID(0), OrderID: ^OrderID(0), Side: Sell, OrdType: StopLimit, Tif: FOK, Flags: FlagPostOnly | FlagIceberg, Price: -1 << 62, StopPrice: 1<<62 - 1, Qty: -1, DisplayQty: 1<<62 - 1, Asset: ^AssetID(0), Amount: -1, ClientReqID: ^uint64(0), ClientTsNanos: -1},
	}
}

func TestEncodeCommandByteIdenticalToReflective(t *testing.T) {
	for i, c := range sampleCommands() {
		want := reflectiveEncode(c)
		got := EncodeCommand(c)
		if !bytes.Equal(got, want) {
			t.Fatalf("command %d: hand-rolled encoding differs from reflective\n got %v\nwant %v", i, got, want)
		}
		if len(got) != CommandSize {
			t.Fatalf("command %d: encoded %d bytes, want CommandSize=%d", i, len(got), CommandSize)
		}
	}
}

func TestEncodeCommandIntoZeroAlloc(t *testing.T) {
	c := sampleCommands()[0]
	buf := make([]byte, CommandSize)
	allocs := testing.AllocsPerRun(1000, func() {
		EncodeCommandInto(buf, c)
	})
	if allocs != 0 {
		t.Fatalf("EncodeCommandInto allocated %.1f allocs/op, want 0", allocs)
	}
}

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
