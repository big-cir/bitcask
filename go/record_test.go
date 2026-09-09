package bitcask

import (
	"bytes"
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
	if h.crc != 0 {
		t.Fatalf("crc = %#x, want 0 while verification is unimplemented", h.crc)
	}

	if got := rec[headerSize : headerSize+len(key)]; !bytes.Equal(got, key) {
		t.Fatalf("key at offset %d = %q, want %q", headerSize, got, key)
	}
	if got := rec[headerSize+len(key):]; !bytes.Equal(got, value) {
		t.Fatalf("value at offset %d = %q, want %q", headerSize+len(key), got, value)
	}
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
