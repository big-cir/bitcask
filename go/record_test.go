package bitcask

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
	"unsafe"
)

func TestHeaderRoundTrip(t *testing.T) {
	want := header{crc: 0xDEADBEEF, seq: 1 << 40, ksz: 1234, vsz: 567890, flags: flagTombstone}

	buf := make([]byte, headerSize)
	want.encode(buf)

	got, err := decodeHeader(buf)
	if err != nil {
		t.Fatalf("decodeHeader: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

// A truncated header must surface as an error, not a panic: a recovery scan
// relies on that to detect a torn tail.
func TestDecodeHeaderTruncated(t *testing.T) {
	if _, err := decodeHeader(make([]byte, headerSize-1)); err == nil {
		t.Fatal("expected an error for a short buffer, got nil")
	}
}

func TestEncodeRecordLayout(t *testing.T) {
	key, value := []byte("k"), []byte("first data")
	rec := encodeRecord(1, key, value, 0)

	if want := headerSize + len(key) + len(value); len(rec) != want {
		t.Fatalf("record size = %d, want %d", len(rec), want)
	}

	h, err := decodeHeader(rec)
	if err != nil {
		t.Fatal(err)
	}
	if h.seq != 1 || h.ksz != uint32(len(key)) || h.vsz != uint32(len(value)) || h.flags != 0 {
		t.Fatalf("unexpected header: %+v", h)
	}

	// The checksum covers everything after its own four bytes, so that damage
	// to a value is caught and not just damage to a header.
	if want := crc32.Checksum(rec[4:], crcTable); h.crc != want {
		t.Fatalf("crc = %#x, want %#x", h.crc, want)
	}

	if got := rec[headerSize : headerSize+len(key)]; !bytes.Equal(got, key) {
		t.Fatalf("key at offset %d = %q, want %q", headerSize, got, key)
	}
	if got := rec[headerSize+len(key):]; !bytes.Equal(got, value) {
		t.Fatalf("value at offset %d = %q, want %q", headerSize+len(key), got, value)
	}
}

func TestDecodeRecordRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   []byte
		value []byte
		flags uint8
	}{
		{"value", []byte("key"), []byte("value"), 0},
		{"empty value", []byte("key"), []byte{}, 0},
		{"tombstone", []byte("key"), nil, flagTombstone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := encodeRecord(7, tc.key, tc.value, tc.flags)

			// A scan hands over the rest of the file, not one record, so
			// decoding must work with trailing bytes present and must report
			// where the record ended.
			src := append(append([]byte{}, enc...), []byte("next record starts here")...)

			rec, n, err := decodeRecord(src, defaultMaxKeySize, defaultMaxValueSize)
			if err != nil {
				t.Fatalf("decodeRecord: %v", err)
			}
			if n != len(enc) {
				t.Fatalf("consumed %d bytes, want %d", n, len(enc))
			}
			if rec.seq != 7 || rec.flags != tc.flags {
				t.Fatalf("got seq=%d flags=%d, want seq=7 flags=%d", rec.seq, rec.flags, tc.flags)
			}
			if !bytes.Equal(rec.key, tc.key) {
				t.Fatalf("key = %q, want %q", rec.key, tc.key)
			}
			if !bytes.Equal(rec.value, tc.value) {
				t.Fatalf("value = %q, want %q", rec.value, tc.value)
			}
			if got := rec.isTombstone(); got != (tc.flags&flagTombstone != 0) {
				t.Fatalf("isTombstone = %v", got)
			}
		})
	}
}

// Damage anywhere in a record must be detected. A checksum over the header
// alone would pass here for every offset past 21 and let a garbled value into
// the index — an engine returning wrong data with no error.
func TestDecodeRecordDetectsDamage(t *testing.T) {
	clean := encodeRecord(1, []byte("k"), []byte("first data"), 0)

	for off := range clean {
		damaged := append([]byte{}, clean...)
		damaged[off] ^= 0xFF

		if _, _, err := decodeRecord(damaged, defaultMaxKeySize, defaultMaxValueSize); err == nil {
			t.Fatalf("byte %d flipped and decodeRecord still accepted the record", off)
		}
	}
}

// The order of the four checks is the point of this test. A half-written
// length field can read as anything, so it has to be rejected before it is
// used as a length: believing it either allocates gigabytes or overflows the
// arithmetic that bounds the record.
func TestDecodeRecordChecksSizesBeforeUsingThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
		want error
	}{
		{"no header at all", []byte("short"), errShortHeader},
		{"ksz beyond the maximum", forgeHeader(0xFFFFFFFF, 0), errBadSize},
		{"vsz beyond the maximum", forgeHeader(1, 0xFFFFFFFF), errBadSize},
		{"body missing", forgeHeader(1, 64), errShortRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := decodeRecord(tc.src, defaultMaxKeySize, defaultMaxValueSize)
			if !errors.Is(err, tc.want) {
				t.Fatalf("decodeRecord = %v, want %v", err, tc.want)
			}
		})
	}
}

// The size limits are configurable up to the whole uint32 range, where
// 21 + ksz + vsz exceeds what a 32-bit int can hold. decodeRecord adds them in
// int64 for that reason; this test states the behaviour the addition has to
// produce, but it cannot fail on a 64-bit build, where int is wide enough
// either way.
func TestDecodeRecordLengthCannotOverflow(t *testing.T) {
	const noLimit = uint32(0xFFFFFFFF)

	_, _, err := decodeRecord(forgeHeader(noLimit, noLimit), noLimit, noLimit)
	if !errors.Is(err, errShortRecord) {
		t.Fatalf("decodeRecord = %v, want errShortRecord", err)
	}
}

// forgeHeader builds a valid, correctly checksummed header with arbitrary
// length fields and no body — the shape a crash leaves behind when it stops
// partway through writing one.
func forgeHeader(ksz, vsz uint32) []byte {
	buf := make([]byte, headerSize)
	h := header{seq: 1, ksz: ksz, vsz: vsz}
	h.encode(buf)
	binary.LittleEndian.PutUint32(buf[0:4], crc32.Checksum(buf[4:], crcTable))
	return buf
}

// The keydir is resident in memory, so the size of one entry sets a ceiling on
// how many keys the engine can hold. Field order is load-bearing.
func TestKeydirEntrySize(t *testing.T) {
	const packed, padded = 24, 32

	if got := unsafe.Sizeof(keydirEntry{}); got != packed {
		t.Fatalf("keydirEntry = %d bytes, want %d; check field order", got, packed)
	}

	// Same fields, reordered so the compiler must insert padding.
	type reordered struct {
		fileID uint32
		vpos   int64
		vsz    uint32
		seq    uint64
	}
	if got := unsafe.Sizeof(reordered{}); got != padded {
		t.Fatalf("reordered = %d bytes, want %d", got, padded)
	}

	t.Logf("packed %d B vs padded %d B: %.1f MB apart at one million keys",
		packed, padded, float64((padded-packed)*1_000_000)/(1<<20))
}
