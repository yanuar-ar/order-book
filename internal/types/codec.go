package types

import (
	"bytes"
	"encoding/binary"
)

// CommandSize is the fixed wire size of an encoded Command: every field written
// in declaration order, packed little-endian, no padding — exactly what
// encoding/binary produces for the struct. The byte layout is a durability
// contract; changing it breaks replay of existing WALs.
const CommandSize = 110

// EncodeCommandInto writes c into dst in the stable little-endian layout and
// returns the number of bytes written. dst must have len >= CommandSize. It
// allocates nothing, so the sequencer can encode into a reusable buffer on the
// hot path. The layout is byte-identical to a reflective
// binary.Write(&buf, LittleEndian, &c) — verified by codec_test.go.
func EncodeCommandInto(dst []byte, c Command) int {
	_ = dst[CommandSize-1] // bounds-check hint: panic early on a short buffer
	le := binary.LittleEndian
	le.PutUint64(dst[0:8], uint64(c.Seq))
	le.PutUint64(dst[8:16], uint64(c.TsNanos))
	dst[16] = uint8(c.Type)
	le.PutUint32(dst[17:21], uint32(c.Market))
	le.PutUint64(dst[21:29], uint64(c.Account))
	le.PutUint64(dst[29:37], uint64(c.OrderID))
	dst[37] = uint8(c.Side)
	dst[38] = uint8(c.OrdType)
	dst[39] = uint8(c.Tif)
	le.PutUint16(dst[40:42], uint16(c.Flags))
	le.PutUint64(dst[42:50], uint64(c.Price))
	le.PutUint64(dst[50:58], uint64(c.StopPrice))
	le.PutUint64(dst[58:66], uint64(c.Qty))
	le.PutUint64(dst[66:74], uint64(c.DisplayQty))
	le.PutUint32(dst[74:78], uint32(c.Asset))
	le.PutUint64(dst[78:86], uint64(c.Amount))
	le.PutUint64(dst[86:94], c.ClientReqID)
	le.PutUint64(dst[94:102], uint64(c.ClientTsNanos))
	le.PutUint64(dst[102:110], c.Epoch)
	return CommandSize
}

// EncodeCommand serializes a Command to a freshly allocated buffer. It is a
// compatibility wrapper over the zero-alloc EncodeCommandInto for callers off
// the hot path (e.g. tests); the byte layout is the WAL durability contract.
func EncodeCommand(c Command) []byte {
	b := make([]byte, CommandSize)
	EncodeCommandInto(b, c)
	return b
}

// DecodeCommand parses a Command produced by EncodeCommand. Replay is not the
// hot path, so this stays on the reflective reader.
func DecodeCommand(p []byte) (Command, error) {
	var c Command
	err := binary.Read(bytes.NewReader(p), binary.LittleEndian, &c)
	return c, err
}
