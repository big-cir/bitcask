package bitcask

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
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

// crcTable is Castagnoli (CRC-32C), not IEEE. Both are 32 bits wide and either
// would detect the same damage, but only Castagnoli compiles down to the
// hardware crc32c instruction on arm64 and amd64. Every byte of every record
// passes through this on the write path and again on every recovery scan, so
// the difference is not academic.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Reasons a byte range is not a record. The recovery scan treats all of them
// the same way — as the end of the intact log — so they exist to make a failed
// scan legible rather than to be handled separately.
var (
	errShortHeader      = errors.New("bitcask: truncated record header")
	errBadSize          = errors.New("bitcask: record length exceeds the configured maximum")
	errShortRecord      = errors.New("bitcask: truncated record body")
	errChecksumMismatch = errors.New("bitcask: record checksum mismatch")
)

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

// record is one decoded record. It exists for the scan paths — recovery, and
// later compaction — which need the key, the value, the sequence number and
// the flags out of a single parse.
//
// There are no ksz, vsz or crc fields. The first two are len(key) and
// len(value), and a second copy of a length is a second thing that can be
// wrong. The checksum is consumed by verification and then has no further use.
//
// record is a transient type. The scan fills the keydir from it and drops it;
// nothing long-lived holds one, which is why replaying a log does not cost as
// much memory as the log is long.
type record struct {
	seq   uint64
	flags uint8
	key   []byte
	value []byte
}

// encodeRecord assembles a complete record — header, key, then value — into a
// single buffer and stamps its checksum.
//
// Emitting one buffer rather than three separate writes is not an
// optimization: it reduces the number of points at which a crash can split a
// record in half.
//
// The checksum covers everything after its own four bytes, seq through the end
// of the value. Covering only the header would leave value corruption
// undetected, and a recovery scan would then admit a garbled value into the
// index — returning wrong data with no error, which is the worst thing a
// storage engine can do.
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

	binary.LittleEndian.PutUint32(buf[0:4], crc32.Checksum(buf[4:], crcTable))

	return buf
}

// decodeRecord parses the record at the start of src and reports how many
// bytes it consumed, so that a scan can locate the next one.
//
// The returned key and value alias src. They are valid only while the caller
// still holds that buffer, which suits a scan that copies what it keeps and
// discards the rest.
//
// The four checks run in this order because each one makes the next one safe:
//
//  1. there are enough bytes to hold a header at all;
//  2. the lengths in that header are within the configured maxima;
//  3. the body those lengths describe is actually present;
//  4. the bytes are the ones that were written.
//
// Check 2 comes before either length is used as a length. A crash can leave a
// half-written vsz that reads as 0xFFFFFFFF, and a scan that trusts it either
// allocates four gigabytes or overflows the addition in check 3. Crashing
// while recovering from a crash is the failure mode with no way out.
func decodeRecord(src []byte, maxKeySize, maxValueSize uint32) (record, int, error) {
	h, err := decodeHeader(src)
	if err != nil {
		return record{}, 0, err
	}
	if h.ksz > maxKeySize || h.vsz > maxValueSize {
		return record{}, 0, errBadSize
	}
	// Computed in int64 because the maxima are configurable up to the whole
	// uint32 range, and a 32-bit int would wrap here and let the comparison
	// below succeed on a length that is nowhere near present.
	total := int64(headerSize) + int64(h.ksz) + int64(h.vsz)
	if total > int64(len(src)) {
		return record{}, 0, errShortRecord
	}

	n := int(total)
	if crc32.Checksum(src[4:n], crcTable) != h.crc {
		return record{}, 0, errChecksumMismatch
	}

	keyEnd := headerSize + int(h.ksz)
	return record{
		seq:   h.seq,
		flags: h.flags,
		key:   src[headerSize:keyEnd],
		value: src[keyEnd:n],
	}, n, nil
}

// isTombstone reports whether r records a deletion rather than a value.
func (r *record) isTombstone() bool { return r.flags&flagTombstone != 0 }
