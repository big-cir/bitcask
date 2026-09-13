package bitcask

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func mustOpen(t *testing.T, dir string, opts ...Option) *DB {
	t.Helper()
	db, err := Open(dir, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func mustClose(t *testing.T, db *DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db := mustOpen(t, dir)
	t.Cleanup(func() { db.Close() })
	return db, dir
}

func mustPut(t *testing.T, db *DB, key, value string) {
	t.Helper()
	if err := db.Put([]byte(key), []byte(value)); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

func assertGet(t *testing.T, db *DB, key, want string) {
	t.Helper()
	got, err := db.Get([]byte(key))
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("Get(%q) = %q, want %q", key, got, want)
	}
}

func assertAbsent(t *testing.T, db *DB, key string) {
	t.Helper()
	if _, err := db.Get([]byte(key)); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get(%q) = %v, want ErrKeyNotFound", key, err)
	}
}

// datafilePaths returns the data files in dir, ascending by id. The last one
// is the active file.
func datafilePaths(t *testing.T, dir string) []string {
	t.Helper()
	ids, err := datafileIDs(dir)
	if err != nil {
		t.Fatalf("datafileIDs: %v", err)
	}
	paths := make([]string, len(ids))
	for i, id := range ids {
		paths[i] = filepath.Join(dir, datafileName(id))
	}
	return paths
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return fi.Size()
}

// ---------- single-session behaviour ----------

func TestPutGet(t *testing.T) {
	db, _ := openTestDB(t)

	mustPut(t, db, "k", "v")
	assertGet(t, db, "k", "v")

	mustPut(t, db, "k", "v2")
	assertGet(t, db, "k", "v2")

	assertAbsent(t, db, "missing")
}

func TestPutGetManyKeys(t *testing.T) {
	db, _ := openTestDB(t)

	const n = 1000
	for i := 0; i < n; i++ {
		mustPut(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
	for i := 0; i < n; i++ {
		assertGet(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
	if got := db.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}
}

func testValue(i int) string {
	return fmt.Sprintf("value-%d-%s", i, bytes.Repeat([]byte("x"), i%37))
}

// An empty value is real data and must round-trip; an empty key is rejected.
// The distinction is what makes the tombstone flag necessary: a deletion is
// also a record with no value bytes.
func TestEmptyKeyAndValue(t *testing.T) {
	db, _ := openTestDB(t)

	if err := db.Put(nil, []byte("v")); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Put with empty key = %v, want ErrEmptyKey", err)
	}
	if err := db.Delete(nil); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Delete with empty key = %v, want ErrEmptyKey", err)
	}

	mustPut(t, db, "empty", "")
	got, err := db.Get([]byte("empty"))
	if err != nil {
		t.Fatalf("Get of empty value: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty value came back as %q", got)
	}
}

// Overwriting a key appends rather than replacing, so the log grows without
// bound until compaction reclaims the superseded records.
func TestOverwriteAppends(t *testing.T) {
	db, dir := openTestDB(t)

	value := bytes.Repeat([]byte("a"), 1024)
	const rounds = 100
	for i := 0; i < rounds; i++ {
		if err := db.Put([]byte("k"), value); err != nil {
			t.Fatal(err)
		}
	}

	size := fileSize(t, filepath.Join(dir, datafileName(firstFileID)))
	recSize := int64(headerSize + len("k") + len(value))
	if want := recSize * rounds; size != want {
		t.Fatalf("file size = %d, want %d", size, want)
	}
	t.Logf("%d bytes on disk hold %d bytes of live data (%.1f%% garbage)",
		size, recSize, 100*float64(size-recSize)/float64(size))
}

func TestDeleteRemovesKey(t *testing.T) {
	db, _ := openTestDB(t)

	mustPut(t, db, "k", "v")
	if err := db.Delete([]byte("k")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertAbsent(t, db, "k")
	if got := db.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}

	// Deleting what is not there writes nothing: a tombstone for an absent key
	// can never shadow anything, so it would be pure garbage.
	if err := db.Delete([]byte("k")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Delete of absent key = %v, want ErrKeyNotFound", err)
	}
}

func TestSizeLimits(t *testing.T) {
	dir := t.TempDir()
	db := mustOpen(t, dir, WithMaxKeySize(8), WithMaxValueSize(16))
	defer db.Close()

	if err := db.Put(bytes.Repeat([]byte("k"), 9), []byte("v")); !errors.Is(err, ErrKeyTooLarge) {
		t.Fatalf("oversized key = %v, want ErrKeyTooLarge", err)
	}
	if err := db.Put([]byte("k"), bytes.Repeat([]byte("v"), 17)); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("oversized value = %v, want ErrValueTooLarge", err)
	}

	mustPut(t, db, "12345678", "1234567890123456")
	assertGet(t, db, "12345678", "1234567890123456")
}

func TestOpenRejectsUnusableOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{"zero MaxFileSize", WithMaxFileSize(0)},
		{"zero MaxKeySize", WithMaxKeySize(0)},
		{"zero MaxValueSize", WithMaxValueSize(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open(t.TempDir(), tc.opt); err == nil {
				t.Fatal("Open accepted the option, want an error")
			}
		})
	}
}

func TestOperationsAfterClose(t *testing.T) {
	db := mustOpen(t, t.TempDir())
	mustClose(t, db)

	if err := db.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close = %v, want ErrClosed", err)
	}
	if _, err := db.Get([]byte("k")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v, want ErrClosed", err)
	}
	if err := db.Delete([]byte("k")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete after Close = %v, want ErrClosed", err)
	}
	if err := db.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close = %v, want ErrClosed", err)
	}
}

// Get hands ownership of the returned slice to the caller, so mutating it must
// not corrupt anything the database still relies on.
func TestGetReturnsOwnedSlice(t *testing.T) {
	db, _ := openTestDB(t)

	mustPut(t, db, "k", "original")
	got, err := db.Get([]byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	copy(got, []byte("MODIFIED"))

	assertGet(t, db, "k", "original")
}

// ---------- recovery ----------

func TestReopenRestoresAllKeys(t *testing.T) {
	dir := t.TempDir()

	const n = 1000
	db := mustOpen(t, dir)
	for i := 0; i < n; i++ {
		mustPut(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
	mustClose(t, db)

	// Nothing about the index was written to disk. Everything below was
	// rebuilt by replaying the log.
	db = mustOpen(t, dir)
	defer db.Close()

	if got := db.Len(); got != n {
		t.Fatalf("Len after reopen = %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		assertGet(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
}

// The tombstone is the whole reason a deletion survives a restart. Recovery
// replays the log, so the original Put record is read again; only a later
// record for the same key can outrank it.
func TestDeleteDoesNotResurrectValue(t *testing.T) {
	dir := t.TempDir()

	db := mustOpen(t, dir)
	mustPut(t, db, "gone", "value")
	mustPut(t, db, "kept", "value")
	if err := db.Delete([]byte("gone")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	mustClose(t, db)

	db = mustOpen(t, dir)
	defer db.Close()

	assertAbsent(t, db, "gone")
	assertGet(t, db, "kept", "value")
	if got := db.Len(); got != 1 {
		t.Fatalf("Len after reopen = %d, want 1", got)
	}
}

func TestRotationAndReopen(t *testing.T) {
	dir := t.TempDir()
	opts := []Option{WithMaxFileSize(4 << 10)}

	const n = 200
	db := mustOpen(t, dir, opts...)
	for i := 0; i < n; i++ {
		mustPut(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
	mustClose(t, db)

	paths := datafilePaths(t, dir)
	if len(paths) < 2 {
		t.Fatalf("got %d data file(s), want several with a 4 KiB limit", len(paths))
	}
	t.Logf("%d keys spread over %d data files", n, len(paths))

	db = mustOpen(t, dir, opts...)
	defer db.Close()

	if got := db.Len(); got != n {
		t.Fatalf("Len after reopen = %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		assertGet(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
}

// If the sequence counter restarted at 1 instead of continuing from the
// highest seq in the log, the second write of a key would tie with the first
// and lose the "highest seq wins" comparison — leaving the stale value in the
// index with no error anywhere.
func TestSeqContinuesAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	db := mustOpen(t, dir)
	mustPut(t, db, "a", "first")
	mustClose(t, db)

	db = mustOpen(t, dir)
	mustPut(t, db, "a", "second")
	mustClose(t, db)

	db = mustOpen(t, dir)
	defer db.Close()
	assertGet(t, db, "a", "second")
}

// A crash leaves a partial record at the end of the file that was being
// written. Those bytes were never acknowledged to a caller, so recovery
// truncates them and carries on; refusing to open would mean a database that
// never survives an ordinary crash.
func TestOpenTruncatesTornTail(t *testing.T) {
	// The three lengths take different paths through the four checks: too
	// short for a header, exactly a header's worth of nonsense, and a header
	// whose lengths point past the end of the file.
	for _, garbage := range []int{5, headerSize, 64} {
		t.Run(fmt.Sprintf("%d bytes", garbage), func(t *testing.T) {
			dir := t.TempDir()

			db := mustOpen(t, dir)
			mustPut(t, db, "before", "value")
			mustClose(t, db)

			paths := datafilePaths(t, dir)
			active := paths[len(paths)-1]
			intact := fileSize(t, active)

			appendBytes(t, active, bytes.Repeat([]byte("g"), garbage))
			if got := fileSize(t, active); got != intact+int64(garbage) {
				t.Fatalf("setup: file is %d bytes, want %d", got, intact+int64(garbage))
			}

			db = mustOpen(t, dir)
			defer db.Close()

			if got := fileSize(t, active); got != intact {
				t.Fatalf("file is %d bytes after recovery, want it truncated to %d", got, intact)
			}
			assertGet(t, db, "before", "value")

			// The truncation has to leave the file appendable. Without it the
			// record below would land on top of the garbage, and the log would
			// be unparseable from that point on — this key would be
			// unreachable after the next restart.
			mustPut(t, db, "after", "value")
			assertGet(t, db, "after", "value")
		})
	}
}

func TestTornTailSurvivesAnotherRestart(t *testing.T) {
	dir := t.TempDir()

	db := mustOpen(t, dir)
	mustPut(t, db, "before", "value")
	mustClose(t, db)

	paths := datafilePaths(t, dir)
	appendBytes(t, paths[len(paths)-1], []byte("garbage on the tail"))

	db = mustOpen(t, dir)
	mustPut(t, db, "after", "value")
	mustClose(t, db)

	db = mustOpen(t, dir)
	defer db.Close()
	assertGet(t, db, "before", "value")
	assertGet(t, db, "after", "value")
}

// corruptSealedFile fills a rotated database, damages one byte inside the
// first sealed data file, and returns the directory. The damage lands in the
// first record of a file that rotation sealed long before the process exited,
// so no crash can account for it.
func corruptSealedFile(t *testing.T, opts ...Option) string {
	t.Helper()
	dir := t.TempDir()

	db := mustOpen(t, dir, opts...)
	for i := 0; i < 100; i++ {
		mustPut(t, db, fmt.Sprintf("key_%04d", i), testValue(i))
	}
	mustClose(t, db)

	paths := datafilePaths(t, dir)
	if len(paths) < 2 {
		t.Fatalf("got %d data file(s), want at least 2", len(paths))
	}
	flipByte(t, paths[0], headerSize+2)
	return dir
}

// The same damage in a sealed file has no crash to explain it: nothing has
// written to that file since it was fsynced. Truncating it would silently
// discard every record after the damage, so Open refuses instead.
//
// The hints are removed first, because recovery only looks at a data file when
// it has no usable hint for it — see the test below.
func TestOpenRejectsCorruptedSealedFile(t *testing.T) {
	opts := []Option{WithMaxFileSize(1 << 10)}
	dir := corruptSealedFile(t, opts...)

	hints, err := filepath.Glob(filepath.Join(dir, "*.hint"))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hints {
		if err := os.Remove(h); err != nil {
			t.Fatal(err)
		}
	}

	db, err := Open(dir, opts...)
	if err == nil {
		db.Close()
		t.Fatal("Open succeeded on a corrupted sealed file")
	}
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Open = %v, want ErrCorrupted", err)
	}
}

// What a hint costs. With a usable hint, recovery never reads the data file,
// so damage inside a sealed data file is not noticed at startup — and Get does
// not check either, because it reads only the value bytes.
//
// This is deliberate rather than an oversight: confirming that every hint
// entry still matches the data file would mean reading the data file, which is
// the work the hint exists to avoid. The damage surfaces the next time
// something reads the file in full, which today means only a scan that had to
// fall back, and later means compaction.
//
// The test exists to pin the behaviour down, not to endorse it.
func TestCorruptionBehindAUsableHintIsNotDetected(t *testing.T) {
	opts := []Option{WithMaxFileSize(1 << 10)}
	dir := corruptSealedFile(t, opts...)

	db, err := Open(dir, opts...)
	if err != nil {
		t.Fatalf("Open = %v, want success: the hint is intact", err)
	}
	defer db.Close()

	if got := db.Len(); got != 100 {
		t.Fatalf("Len = %d, want 100", got)
	}
	// The damaged bytes are part of a key, which lives only in the data file.
	// The index got that key from the hint, and the value it points at is
	// untouched, so reads come back correct.
	assertGet(t, db, "key_0000", testValue(0))
}

// A stray file this package did not write is left alone rather than treated as
// a reason to refuse the whole directory.
func TestOpenIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()

	db := mustOpen(t, dir)
	mustPut(t, db, "k", "v")
	mustClose(t, db)

	if err := os.WriteFile(filepath.Join(dir, "notes.data"), []byte("not a log"), 0o644); err != nil {
		t.Fatal(err)
	}

	db = mustOpen(t, dir)
	defer db.Close()
	assertGet(t, db, "k", "v")
}

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for flip: %v", err)
	}
	defer f.Close()

	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatalf("read at %d: %v", off, err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatalf("write at %d: %v", off, err)
	}
}

// The ordering rule is "highest seq for a key wins", and nothing in normal
// operation can tell it apart from "whatever was written last wins": records
// are appended in seq order and files are read in id order, so the two rules
// agree on every database this package produces today.
//
// They stop agreeing the moment compaction rewrites old records into new
// files, which is what v3 does. A recovery built on file order would pass
// every other test in this suite and then return stale values, with no error,
// once merging starts.
//
// These files are therefore assembled by hand, with seq deliberately out of
// step with position. The scan path and the hint path are both checked, since
// each applies the rule itself.
func TestHighestSeqWinsNotFilePosition(t *testing.T) {
	t.Run("within one file", func(t *testing.T) {
		dir := t.TempDir()

		var buf []byte
		buf = append(buf, encodeRecord(2, []byte("k"), []byte("newer"), 0)...)
		buf = append(buf, encodeRecord(1, []byte("k"), []byte("older"), 0)...)
		writeDatafile(t, dir, 1, buf)

		db := mustOpen(t, dir)
		defer db.Close()
		// File order says "older". Sequence order says "newer".
		assertGet(t, db, "k", "newer")
	})

	t.Run("across files", func(t *testing.T) {
		dir := t.TempDir()

		// The lower file id holds the newer record, which is exactly the shape
		// compaction produces: merged output is written to a fresh file while
		// carrying the sequence numbers of the records it copied.
		writeDatafile(t, dir, 1, encodeRecord(9, []byte("k"), []byte("newer"), 0))
		writeDatafile(t, dir, 2, encodeRecord(4, []byte("k"), []byte("older"), 0))

		db := mustOpen(t, dir)
		defer db.Close()
		assertGet(t, db, "k", "newer")
	})

	t.Run("a tombstone with a lower seq must not delete", func(t *testing.T) {
		dir := t.TempDir()

		writeDatafile(t, dir, 1, encodeRecord(7, []byte("k"), []byte("alive"), 0))
		writeDatafile(t, dir, 2, encodeRecord(3, []byte("k"), nil, flagTombstone))

		db := mustOpen(t, dir)
		defer db.Close()
		// The tombstone comes later in the directory but is older. Position
		// would delete the key; sequence keeps it.
		assertGet(t, db, "k", "alive")
	})

	t.Run("through the hint path", func(t *testing.T) {
		dir := t.TempDir()

		writeDatafile(t, dir, 1, encodeRecord(9, []byte("k"), []byte("newer"), 0))
		writeDatafile(t, dir, 2, encodeRecord(4, []byte("k"), []byte("older"), 0))
		// File 2 is the active file and is always scanned, so put the older
		// record behind a hint by adding a third file to take that role.
		writeDatafile(t, dir, 3, encodeRecord(11, []byte("other"), []byte("v"), 0))

		opts := &options{maxFileSize: defaultMaxFileSize, maxKeySize: defaultMaxKeySize, maxValueSize: defaultMaxValueSize}
		for _, id := range []uint32{1, 2} {
			if err := buildHint(dir, id, opts); err != nil {
				t.Fatalf("buildHint %d: %v", id, err)
			}
		}
		if _, err := loadHint(dir, 2, defaultMaxKeySize); err != nil {
			t.Fatalf("hint for file 2 unusable, so this would not test the hint path: %v", err)
		}

		db := mustOpen(t, dir)
		defer db.Close()
		assertGet(t, db, "k", "newer")
	})
}

// writeDatafile plants a data file with exactly the bytes given, so that a
// test can build a history the write path would never produce.
func writeDatafile(t *testing.T, dir string, id uint32, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, datafileName(id)), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Which copy the index points at when two records share a seq.
//
// By design that cannot happen to two different records: sequence numbers are
// never reused, so equal seq means the same record written twice. Both copies
// hold the same value, so reads are unaffected either way — which is why
// changing applyEntry's comparison from >= to > breaks no test.
//
// It stops being unobservable in v3. Compaction copies records into new files
// keeping their original seq, so an interrupted merge leaves two copies in
// different files, and merge's conditional update compares the (fileID, vpos)
// the index is holding. This test records the current tie-break rather than
// claiming it is the right one: the first copy in file id order wins.
//
// If v3 needs the other rule, changing it here is a decision, not a fix.
func TestDuplicateSeqKeepsTheFirstCopy(t *testing.T) {
	dir := t.TempDir()

	rec := encodeRecord(5, []byte("k"), []byte("same value"), 0)
	writeDatafile(t, dir, 1, rec)
	writeDatafile(t, dir, 2, rec)

	db := mustOpen(t, dir)
	defer db.Close()

	// Either copy returns the same bytes. Only the address differs.
	assertGet(t, db, "k", "same value")

	e, ok := db.keydir.get([]byte("k"))
	if !ok {
		t.Fatal("key missing")
	}
	if e.fileID != 1 {
		t.Fatalf("index points at file %d, want 1 (the first copy)", e.fileID)
	}
}
