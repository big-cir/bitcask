package bitcask

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func TestHintRecordRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  hintRecord
	}{
		{"value", hintRecord{seq: 9, vpos: 1234, vsz: 56, flags: 0, key: []byte("user:1")}},
		{"empty value", hintRecord{seq: 1, vpos: 21, vsz: 0, flags: 0, key: []byte("k")}},
		{"tombstone", hintRecord{seq: 1 << 40, vpos: 1 << 40, vsz: 0, flags: flagTombstone, key: []byte("gone")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := tc.rec.encode()
			if want := hintHeaderSize + len(tc.rec.key); len(enc) != want {
				t.Fatalf("encoded size = %d, want %d", len(enc), want)
			}

			// A load hands over the rest of the file, not one record, so
			// decoding must work with trailing bytes and report where the
			// record ended.
			src := append(append([]byte{}, enc...), []byte("the next hint record")...)

			got, n, err := decodeHintRecord(src, defaultMaxKeySize)
			if err != nil {
				t.Fatalf("decodeHintRecord: %v", err)
			}
			if n != len(enc) {
				t.Fatalf("consumed %d bytes, want %d", n, len(enc))
			}
			if got.seq != tc.rec.seq || got.vpos != tc.rec.vpos || got.vsz != tc.rec.vsz || got.flags != tc.rec.flags {
				t.Fatalf("got %+v, want %+v", got, tc.rec)
			}
			if !bytes.Equal(got.key, tc.rec.key) {
				t.Fatalf("key = %q, want %q", got.key, tc.rec.key)
			}
		})
	}
}

func TestDecodeHintRecordDetectsDamage(t *testing.T) {
	rec := hintRecord{seq: 3, vpos: 512, vsz: 40, key: []byte("user:1")}
	clean := rec.encode()

	for off := range clean {
		damaged := append([]byte{}, clean...)
		damaged[off] ^= 0xFF

		if _, _, err := decodeHintRecord(damaged, defaultMaxKeySize); err == nil {
			t.Fatalf("byte %d flipped and decodeHintRecord still accepted the record", off)
		}
	}
}

func TestDecodeHintRecordChecks(t *testing.T) {
	forge := func(ksz uint32) []byte {
		buf := make([]byte, hintHeaderSize)
		binary.LittleEndian.PutUint64(buf[4:12], 1)
		binary.LittleEndian.PutUint32(buf[12:16], ksz)
		binary.LittleEndian.PutUint32(buf[0:4], crc32.Checksum(buf[4:], crcTable))
		return buf
	}

	for _, tc := range []struct {
		name string
		src  []byte
		want error
	}{
		{"no header at all", []byte("short"), errShortHint},
		{"zero ksz", forge(0), errBadSize},
		{"ksz beyond the maximum", forge(0xFFFFFFFF), errBadSize},
		{"key missing", forge(16), errShortHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := decodeHintRecord(tc.src, defaultMaxKeySize); !errors.Is(err, tc.want) {
				t.Fatalf("decodeHintRecord = %v, want %v", err, tc.want)
			}
		})
	}
}

// keydirSnapshot copies the whole index out, so that two of them can be
// compared entry by entry.
func keydirSnapshot(db *DB) map[string]keydirEntry {
	db.keydir.mu.RLock()
	defer db.keydir.mu.RUnlock()

	out := make(map[string]keydirEntry, len(db.keydir.m))
	for k, e := range db.keydir.m {
		out[k] = e
	}
	return out
}

// loadRotated fills a database spread over several data files and returns the
// directory, the options it was opened with, and how many keys are live.
func loadRotated(t *testing.T, keys int) (string, []Option) {
	t.Helper()
	dir := t.TempDir()
	opts := []Option{WithMaxFileSize(4 << 10)}

	db := mustOpen(t, dir, opts...)
	for i := 0; i < keys/2; i++ {
		mustPut(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
	// A deletion and an overwrite, placed here rather than at the end on
	// purpose: the writes that follow push these records into sealed files, so
	// hints are what carry them. Putting them last would leave them in the
	// active file, which is always scanned — the hint path would never see a
	// tombstone at all and a bug in its handling would go unnoticed.
	if err := db.Delete([]byte("key_0007")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	mustPut(t, db, "key_0011", "overwritten")
	for i := keys / 2; i < keys; i++ {
		mustPut(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
	mustClose(t, db)

	return dir, opts
}

func hintPaths(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.hint"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// Hints are written when a file is sealed, and only then. The file still being
// appended to has no hint, because a summary of it would be stale immediately.
func TestRotationWritesHintForSealedFilesOnly(t *testing.T) {
	dir, _ := loadRotated(t, 200)

	data := datafilePaths(t, dir)
	hints := hintPaths(t, dir)
	if len(data) < 2 {
		t.Fatalf("got %d data file(s), want several", len(data))
	}
	if want := len(data) - 1; len(hints) != want {
		t.Fatalf("got %d hint file(s) for %d data file(s), want %d", len(hints), len(data), want)
	}

	activeID, err := datafileIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, hintName(activeID[len(activeID)-1]))
	if _, err := os.Stat(active); !os.IsNotExist(err) {
		t.Fatalf("the active file has a hint: %s", filepath.Base(active))
	}

	t.Logf("%d data files, %d hint files", len(data), len(hints))
	for _, h := range hints {
		d := h[:len(h)-len(".hint")] + ".data"
		t.Logf("  %s %6d B   %s %6d B   ratio %.3f",
			filepath.Base(d), fileSize(t, d), filepath.Base(h), fileSize(t, h),
			float64(fileSize(t, h))/float64(fileSize(t, d)))
	}
}

// The point of v2. A hint file's value offsets are stored, while a data file
// scan derives them from how far it has read. Nothing else in the suite would
// notice if those two disagreed: a wrong vpos makes Get return neighbouring
// bytes, not an error.
func TestKeydirIdenticalWithAndWithoutHints(t *testing.T) {
	dir, opts := loadRotated(t, 200)

	// Assert the hint path is actually taken. Recovery falls back to the data
	// file whenever a hint is unusable, and that fallback is silent — without
	// this check a hint bug would be rejected, both snapshots would come from
	// the scan path, and the comparison below would compare the scan path to
	// itself and pass.
	requireHintsUsable(t, dir)

	withHints := mustOpen(t, dir, opts...)
	fromHints := keydirSnapshot(withHints)
	mustClose(t, withHints)

	for _, p := range hintPaths(t, dir) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}

	withoutHints := mustOpen(t, dir, opts...)
	fromScan := keydirSnapshot(withoutHints)
	mustClose(t, withoutHints)

	compareKeydirs(t, fromHints, fromScan)
	t.Logf("%d entries identical through both paths", len(fromScan))
}

// requireHintsUsable fails the test unless every sealed data file has a hint
// that loadHint accepts.
func requireHintsUsable(t *testing.T, dir string) {
	t.Helper()
	ids, err := datafileIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) < 2 {
		t.Fatalf("got %d data file(s), want several so that some are sealed", len(ids))
	}
	for _, id := range ids[:len(ids)-1] {
		records, err := loadHint(dir, id, defaultMaxKeySize)
		if err != nil {
			t.Fatalf("%s is not usable, so recovery would fall back and this test would prove nothing: %v",
				hintName(id), err)
		}
		if len(records) == 0 {
			t.Fatalf("%s summarises no records", hintName(id))
		}
	}
}

func compareKeydirs(t *testing.T, a, b map[string]keydirEntry) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("entry count: hint path %d, scan path %d", len(a), len(b))
	}
	for k, want := range b {
		got, ok := a[k]
		if !ok {
			t.Fatalf("key %q present through the scan path but missing through the hint path", k)
		}
		if got != want {
			t.Fatalf("key %q: hint path %+v, scan path %+v", k, got, want)
		}
	}
}

// A hint is an optimization, so every way of ruining one has to end in the
// same index, reached by reading the data file instead.
func TestOpenFallsBackOnUnusableHint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, path string)
	}{
		{"deleted", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		// Cut mid-record: the last record left is short, so decoding it
		// fails.
		{"truncated mid-record", func(t *testing.T, path string) {
			truncateAt(t, path, fileSize(t, path)/3)
		}},
		// Cut exactly on a record boundary, which is the case per-record
		// checks cannot see: everything left decodes and every checksum
		// matches. Only the coverage check against the data file catches it.
		// The keys here are 8 bytes, so a record is hintHeaderSize + 8.
		{"truncated on a record boundary", func(t *testing.T, path string) {
			const recSize = int64(hintHeaderSize + len("key_0000"))
			whole := fileSize(t, path) / recSize
			if whole < 2 {
				t.Fatalf("hint holds %d record(s), need several", whole)
			}
			truncateAt(t, path, whole/2*recSize)
		}},
		{"one byte flipped", func(t *testing.T, path string) {
			flipByte(t, path, 12)
		}},
		{"garbage appended", func(t *testing.T, path string) {
			appendBytes(t, path, []byte("this is not a hint record"))
		}},
		{"emptied", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, opts := loadRotated(t, 200)

			intact := mustOpen(t, dir, opts...)
			want := keydirSnapshot(intact)
			mustClose(t, intact)

			hints := hintPaths(t, dir)
			if len(hints) == 0 {
				t.Fatal("no hint files were written")
			}
			tc.break_(t, hints[0])

			db, err := Open(dir, opts...)
			if err != nil {
				t.Fatalf("Open failed on an unusable hint: %v", err)
			}
			defer db.Close()

			compareKeydirs(t, keydirSnapshot(db), want)
			assertGet(t, db, "key_0011", "overwritten")
			assertAbsent(t, db, "key_0007")
		})
	}
}

// Hints carry seq, so the counter has to continue from them just as it does
// from a full scan. If it restarted, the next write of a key would tie with
// the record already in the index and lose.
func TestSeqContinuesThroughHints(t *testing.T) {
	dir, opts := loadRotated(t, 200)

	db := mustOpen(t, dir, opts...)
	mustPut(t, db, "key_0000", "written after the restart")
	mustClose(t, db)

	db = mustOpen(t, dir, opts...)
	defer db.Close()
	assertGet(t, db, "key_0000", "written after the restart")
}

func truncateAt(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

// A hint file that decodes cleanly can still be wrong. Every per-record check
// passes on a hint cut at a record boundary, and the index it produces is
// missing every key past the cut with nothing reporting it. This test exists
// because the first version of the truncation case above cut at size/3, landed
// mid-record by luck, and passed while 37 keys silently disappeared.
func TestHintCutOnRecordBoundaryIsRejected(t *testing.T) {
	dir, opts := loadRotated(t, 200)

	intact := mustOpen(t, dir, opts...)
	want := keydirSnapshot(intact)
	mustClose(t, intact)

	path := hintPaths(t, dir)[0]
	const recSize = int64(hintHeaderSize + len("key_0000"))
	whole := fileSize(t, path) / recSize
	cut := whole / 2 * recSize
	truncateAt(t, path, cut)

	// Every record still in the file decodes, so nothing below the file level
	// notices.
	var decoded int
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(data); {
		_, n, err := decodeHintRecord(data[off:], defaultMaxKeySize)
		if err != nil {
			t.Fatalf("record at %d failed to decode, so this is not the case under test: %v", off, err)
		}
		decoded++
		off += n
	}
	if want := int(whole / 2); decoded != want {
		t.Fatalf("%d records decoded, want %d", decoded, want)
	}

	if _, err := loadHint(dir, 1, defaultMaxKeySize); !errors.Is(err, errIncompleteHint) {
		t.Fatalf("loadHint = %v, want errIncompleteHint", err)
	}

	db := mustOpen(t, dir, opts...)
	defer db.Close()
	compareKeydirs(t, keydirSnapshot(db), want)
	t.Logf("%d of %d hint records survived the cut and all of them decoded; "+
		"the coverage check rejected the file and all %d keys came back",
		decoded, whole, len(want))
}

// Hint files are put in place with a rename, so a crash while one is being
// written leaves either no hint or a complete one — never a shorter file that
// happens to parse.
func TestWriteHintIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, datafileName(1)), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeHint(dir, 1, nil); err != nil {
		t.Fatalf("writeHint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, hintTempName(1))); !os.IsNotExist(err) {
		t.Fatal("the temporary file outlived the write")
	}

	// A leftover temporary file is what a crash mid-write looks like. It must
	// not be mistaken for a hint, and Open clears it away.
	tmp := filepath.Join(dir, hintTempName(1))
	if err := os.WriteFile(tmp, []byte("half a hint file"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := mustOpen(t, dir)
	defer db.Close()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("Open left a stale .hint.tmp behind")
	}
}

// A tombstone has to survive the hint path, not just the scan path. The flags
// byte is the only thing that distinguishes it, and recovery has to drop the
// key rather than index a zero-length value.
func TestTombstoneSurvivesTheHintPath(t *testing.T) {
	dir, opts := loadRotated(t, 200)
	requireHintsUsable(t, dir)

	// The tombstone must be in a sealed file, or the scan path would be the
	// one handling it and this test would not be testing the hint path.
	found := false
	ids, err := datafileIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[:len(ids)-1] {
		records, err := loadHint(dir, id, defaultMaxKeySize)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range records {
			if string(h.key) == "key_0007" && h.flags&flagTombstone != 0 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no tombstone for key_0007 in any hint file; the fixture is not exercising the hint path")
	}

	db := mustOpen(t, dir, opts...)
	defer db.Close()
	assertAbsent(t, db, "key_0007")
	assertGet(t, db, "key_0011", "overwritten")
}
