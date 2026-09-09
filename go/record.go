package bitcask

import (
	"encoding/binary"
	"errors"
)

// headerSize is the size of the fixed-length record header in bytes.
//
//	0        4              12        16        20     21
//	├────────┼──────────────┼─────────┼─────────┼──────┼───────┬─────────┐
//	│ crc32  │     seq      │   ksz   │   vsz   │flags │  key  │  value  │
//	└────────┴──────────────┴─────────┴─────────┴──────┴───────┴─────────┘
//	└──────────── fixed 21 bytes ─────────────────┘└─ variable length ─┘
const headerSize = 21

// Record header flags.
const (
	// flagTombstone marks a deletion. A tombstone record has vsz == 0 and no
	// value bytes. Deletion is a flag rather than a zero-length value because
	// an empty value is legitimate data and must remain distinguishable.
	flagTombstone uint8 = 1 << iota
)

var errShortHeader = errors.New("bitcask: truncated record header")

// header is the fixed-size portion of a record.
//
// It intentionally has no key or value fields. Go struct sizes are fixed at
// compile time, so variable-length data cannot be embedded; keys and values
// are handled as byte slices, and what reaches the disk is always the buffer
// produced by encodeRecord.
type header struct {
	crc   uint32
	seq   uint64
	ksz   uint32
	vsz   uint32
	flags uint8
}

// encode writes h into the first headerSize bytes of dst, which must be at
// least that long.
//
// Field offsets appear only here and in decodeHeader. Spreading them across
// call sites would let one side drift from the other without the round-trip
// test noticing.
func (h *header) encode(dst []byte) {
	binary.LittleEndian.PutUint32(dst[0:4], h.crc)
	binary.LittleEndian.PutUint64(dst[4:12], h.seq)
	binary.LittleEndian.PutUint32(dst[12:16], h.ksz)
	binary.LittleEndian.PutUint32(dst[16:20], h.vsz)
	dst[20] = h.flags
}

// decodeHeader parses a record header from the start of src. It returns
// errShortHeader if src is too small, which a recovery scan treats as a
// truncated tail rather than a fatal error.
func decodeHeader(src []byte) (header, error) {
	if len(src) < headerSize {
		return header{}, errShortHeader
	}
	return header{
		crc:   binary.LittleEndian.Uint32(src[0:4]),
		seq:   binary.LittleEndian.Uint64(src[4:12]),
		ksz:   binary.LittleEndian.Uint32(src[12:16]),
		vsz:   binary.LittleEndian.Uint32(src[16:20]),
		flags: src[20],
	}, nil
}

// encodeRecord assembles a complete record — header, key, then value — into a
// single buffer.
//
// Emitting one buffer rather than three separate writes is not an
// optimization: it reduces the number of points at which a crash can split a
// record in half.
//
// The checksum field is left zero; verification is not yet implemented.
func encodeRecord(seq uint64, key, value []byte, flags uint8) []byte {
	buf := make([]byte, headerSize+len(key)+len(value))

	h := header{
		seq:   seq,
		ksz:   uint32(len(key)),
		vsz:   uint32(len(value)),
		flags: flags,
	}
	h.encode(buf[:headerSize])
	copy(buf[headerSize:], key)
	copy(buf[headerSize+len(key):], value)

	return buf
}
