package bitcask

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dir
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

func TestPutGet(t *testing.T) {
	db, _ := openTestDB(t)

	if err := db.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	assertGet(t, db, "k", "v")

	if err := db.Put([]byte("k"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	assertGet(t, db, "k", "v2")

	if _, err := db.Get([]byte("missing")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get of absent key = %v, want ErrKeyNotFound", err)
	}
}

func TestPutGetManyKeys(t *testing.T) {
	db, _ := openTestDB(t)

	const n = 1000
	value := func(i int) string {
		return fmt.Sprintf("value-%d-%s", i, bytes.Repeat([]byte("x"), i%37))
	}

	for i := 0; i < n; i++ {
		if err := db.Put([]byte(fmt.Sprintf("key_%04d", i)), []byte(value(i))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		assertGet(t, db, fmt.Sprintf("key_%04d", i), value(i))
	}
	if got := db.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}
}

// An empty value is real data and must round-trip; an empty key is rejected.
// The distinction matters once deletion is introduced, because a tombstone is
// also a record with no value bytes.
func TestEmptyKeyAndValue(t *testing.T) {
	db, _ := openTestDB(t)

	if err := db.Put(nil, []byte("v")); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Put with empty key = %v, want ErrEmptyKey", err)
	}

	if err := db.Put([]byte("empty"), []byte{}); err != nil {
		t.Fatalf("Put with empty value: %v", err)
	}
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

	fi, err := os.Stat(filepath.Join(dir, datafileName(firstFileID)))
	if err != nil {
		t.Fatal(err)
	}
	recSize := int64(headerSize + len("k") + len(value))
	if want := recSize * rounds; fi.Size() != want {
		t.Fatalf("file size = %d, want %d", fi.Size(), want)
	}
	t.Logf("%d bytes on disk hold %d bytes of live data (%.1f%% garbage)",
		fi.Size(), recSize, 100*float64(fi.Size()-recSize)/float64(fi.Size()))
}

func TestOpenRejectsExistingData(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(dir); !errors.Is(err, ErrExistingData) {
		t.Fatalf("reopen = %v, want ErrExistingData", err)
	}
}

func TestOperationsAfterClose(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := db.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close = %v, want ErrClosed", err)
	}
	if _, err := db.Get([]byte("k")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v, want ErrClosed", err)
	}
	if err := db.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close = %v, want ErrClosed", err)
	}
}

// Get hands ownership of the returned slice to the caller, so mutating it must
// not corrupt anything the database still relies on.
func TestGetReturnsOwnedSlice(t *testing.T) {
	db, _ := openTestDB(t)

	if err := db.Put([]byte("k"), []byte("original")); err != nil {
		t.Fatal(err)
	}
	got, err := db.Get([]byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	copy(got, []byte("MODIFIED"))

	assertGet(t, db, "k", "original")
}
