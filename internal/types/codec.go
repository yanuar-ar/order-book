package types

import (
	"bytes"
	"encoding/binary"
)

// EncodeCommand serializes a Command to a fixed little-endian byte layout for
// the WAL. Every field is a fixed-size scalar, so the encoding is stable across
// runs. (The performance phase may replace this reflective path with a manual
// encoder; the byte layout is the contract.)
func EncodeCommand(c Command) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, &c)
	return b.Bytes()
}

// DecodeCommand parses a Command produced by EncodeCommand.
func DecodeCommand(p []byte) (Command, error) {
	var c Command
	err := binary.Read(bytes.NewReader(p), binary.LittleEndian, &c)
	return c, err
}
