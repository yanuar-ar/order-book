package types

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzCommandCodec drives arbitrary field bit-patterns through the codec and
// asserts two invariants: the hand-rolled encoding is byte-identical to the
// reflective reference (the WAL durability contract), and decode round-trips.
// Seeded from a raw 102-byte command buffer so every field takes arbitrary
// values, including ones the typed generators never produce.
func FuzzCommandCodec(f *testing.F) {
	f.Add(EncodeCommand(sampleCommands()[0]))
	f.Add(EncodeCommand(sampleCommands()[len(sampleCommands())-1]))
	f.Add(make([]byte, CommandSize)) // all-zero command

	f.Fuzz(func(t *testing.T, data []byte) {
		// Normalize to a full command buffer, then decode arbitrary bytes into a
		// Command so every field carries a fuzzed value.
		buf := make([]byte, CommandSize)
		copy(buf, data)
		var c Command
		if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &c); err != nil {
			t.Fatalf("decode of a CommandSize buffer must not error: %v", err)
		}

		// Byte-identity: hand-rolled == reflective.
		if got, want := EncodeCommand(c), reflectiveEncode(c); !bytes.Equal(got, want) {
			t.Fatalf("hand-rolled encoding diverged from reflective for %+v\n got %v\nwant %v", c, got, want)
		}
		// Round-trip: decode(encode(c)) == c.
		if got, err := DecodeCommand(EncodeCommand(c)); err != nil || got != c {
			t.Fatalf("round-trip failed (err=%v):\n got %+v\nwant %+v", err, got, c)
		}
	})
}
