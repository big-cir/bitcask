package bitcask

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Damage sweeps.
//
// The rest of the suite checks cases chosen by hand, which means it checks the
// failures somebody thought of. These tests choose nothing: they damage a file
// at every position there is and check the same strict condition each time.
//
// That difference is not academic. The hint truncation case in hint_test.go
// was written by hand, cut the file at size/3, landed mid-record by luck, and
// passed while 37 keys silently disappeared. A sweep over every offset would
// have failed on the first run.

// damagedDB builds a small rotated database and returns the directory, the
// options, and the index a clean Open produces.
//
// The files are deliberately tiny. Each sweep below reopens the database once
// per damaged byte, so the first sealed file's size is what these tests cost.
// It still has to hold several records, a tombstone and an overwrite, or the
// sweep would be damaging a file with nothing interesting in it.
func damagedDB(t *testing.T) (string, []Option, map[string]keydirEntry) {
	t.Helper()
	dir := t.TempDir()
	opts := []Option{WithMaxFileSize(600)}

	db := mustOpen(t, dir, opts...)
	for i := 0; i < 20; i++ {
		mustPut(t, db, fmt.Sprintf("key_%02d", i), fmt.Sprintf("value-%02d", i))
	}
	// Placed here rather than last so the records that follow push them into a
	// sealed file, which is what the sweeps damage.
	if err := db.Delete([]byte("key_03")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	mustPut(t, db, "key_07", "overwritten")
	for i := 20; i < 40; i++ {
		mustPut(t, db, fmt.Sprintf("key_%02d", i), fmt.Sprintf("value-%02d", i))
	}
	index := keydirSnapshot(db)
	mustClose(t, db)

	if len(hintPaths(t, dir)) == 0 {
		t.Fatalf("no hint files were written; %d data file(s)", len(datafilePaths(t, dir)))
	}
	return dir, opts, index
}

// assertIndexUnchanged reopens the database and requires the index to match
// want exactly. Every value has to read back, too: a wrong vpos produces
// neighbouring bytes rather than a missing key.
func assertIndexUnchanged(t *testing.T, dir string, opts []Option, want map[string]keydirEntry, what string) {
	t.Helper()
	db, err := Open(dir, opts...)
	if err != nil {
		t.Fatalf("%s: Open failed: %v", what, err)
	}
	defer db.Close()

	got := keydirSnapshot(db)
	if len(got) != len(want) {
		t.Fatalf("%s: index holds %d entries, want %d", what, len(got), len(want))
	}
	for key, wantEntry := range want {
		gotEntry, ok := got[key]
		if !ok {
			t.Fatalf("%s: key %q is gone", what, key)
		}
		if gotEntry != wantEntry {
			t.Fatalf("%s: key %q at %+v, want %+v", what, key, gotEntry, wantEntry)
		}
	}
}

// A hint file is derived from a data file, so no damage to one can change what
// the database holds. Every truncation point and every single-byte flip has to
// end in the same index, reached by reading the data file instead.
func TestDamageSweepHintFile(t *testing.T) {
	dir, opts, want := damagedDB(t)
	path := hintPaths(t, dir)[0]

	intact, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	stride := sweepStride()
	var cuts, flips int
	for n := 0; n <= len(intact); n += stride {
		if err := os.WriteFile(path, intact[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		assertIndexUnchanged(t, dir, opts, want, fmt.Sprintf("hint truncated to %d/%d bytes", n, len(intact)))
		cuts++
	}

	for i := 0; i < len(intact); i += stride {
		damaged := append([]byte{}, intact...)
		damaged[i] ^= 0xFF
		if err := os.WriteFile(path, damaged, 0o644); err != nil {
			t.Fatal(err)
		}
		assertIndexUnchanged(t, dir, opts, want, fmt.Sprintf("hint byte %d flipped", i))
		flips++
	}

	t.Logf("%d truncation points and %d byte flips over %s (stride %d), index identical every time",
		cuts, flips, filepath.Base(path), stride)
}

// A byte flipped inside a sealed data file has to be caught, because nothing
// wrote to that file after it was fsynced. The hints are removed first, since
// recovery does not read a data file it has a usable hint for.
func TestDamageSweepSealedDatafileFlips(t *testing.T) {
	dir, opts, _ := damagedDB(t)
	for _, p := range hintPaths(t, dir) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := datafileIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, datafileName(ids[0]))
	intact, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	stride := sweepStride()
	var flips int
	for i := 0; i < len(intact); i += stride {
		damaged := append([]byte{}, intact...)
		damaged[i] ^= 0xFF
		if err := os.WriteFile(path, damaged, 0o644); err != nil {
			t.Fatal(err)
		}

		db, err := Open(dir, opts...)
		if err == nil {
			db.Close()
			t.Fatalf("byte %d of %s flipped and Open accepted it", i, datafileName(ids[0]))
		}
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("byte %d flipped: Open = %v, want ErrCorrupted", i, err)
		}
		flips++
	}
	t.Logf("%d byte flips over %s (stride %d), every one reported as corruption",
		flips, filepath.Base(path), stride)
}

// Truncating a sealed data file, at every offset.
//
// Two outcomes are allowed and the offset decides which:
//
//   - cut inside a record: the last record left is short or mis-checksummed,
//     so Open refuses with ErrCorrupted;
//   - cut exactly on a record boundary: every record left is intact, so the
//     file is indistinguishable from a shorter file that was always that
//     length. Open accepts it and the keys past the cut are gone.
//
// The second case is a real limitation and this test states it rather than
// hiding it. Detecting it needs the expected length of the file recorded
// somewhere outside the file, which this format does not have — a hint file
// cannot serve, because a hint is derived data that may be absent or stale.
//
// What is checked either way is that no reachable key reads back wrong. Losing
// keys to a truncated file is unavoidable; returning the wrong bytes for a key
// that is still there would not be.
func TestDamageSweepSealedDatafileTruncation(t *testing.T) {
	dir, opts, want := damagedDB(t)
	for _, p := range hintPaths(t, dir) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := datafileIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, datafileName(ids[0]))
	intact, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	boundaries := recordBoundaries(t, intact)

	// The values every surviving key must still read back as.
	values := map[string][]byte{}
	full := mustOpen(t, dir, opts...)
	for key := range want {
		v, err := full.Get([]byte(key))
		if err != nil {
			t.Fatal(err)
		}
		values[key] = v
	}
	mustClose(t, full)

	stride := sweepStride()
	var accepted, refused int
	for n := 0; n <= len(intact); n += stride {
		if err := os.WriteFile(path, intact[:n], 0o644); err != nil {
			t.Fatal(err)
		}

		db, err := Open(dir, opts...)
		onBoundary := boundaries[n]

		if err != nil {
			if onBoundary {
				t.Fatalf("cut at %d is a record boundary, so the file is well formed, but Open = %v", n, err)
			}
			if !errors.Is(err, ErrCorrupted) {
				t.Fatalf("cut at %d: Open = %v, want ErrCorrupted", n, err)
			}
			refused++
			continue
		}
		if !onBoundary {
			db.Close()
			t.Fatalf("cut at %d lands inside a record and Open accepted it", n)
		}
		accepted++

		// Keys may be missing. The ones that are left must be right.
		for key, e := range keydirSnapshot(db) {
			v, err := db.Get([]byte(key))
			if err != nil {
				db.Close()
				t.Fatalf("cut at %d: key %q is indexed at %+v but unreadable: %v", n, key, e, err)
			}
			if !bytes.Equal(v, values[key]) {
				db.Close()
				t.Fatalf("cut at %d: key %q = %q, want %q", n, key, v, values[key])
			}
		}
		db.Close()
	}

	t.Logf("%d truncation points over %s (stride %d): %d refused, %d accepted",
		refused+accepted, filepath.Base(path), stride, refused, accepted)
	t.Logf("the %d accepted cuts all land on record boundaries, where a shorter file "+
		"is indistinguishable from a complete one; keys past the cut are lost and "+
		"nothing in this format can report it", accepted)
}

// sweepStride is how many offsets a sweep advances at a time.
//
// Exhaustive is the point of these tests, so the stride is 1 by default. Under
// -short it samples instead, which keeps a quick run quick while still
// covering both sides of every kind of boundary. Run the full sweep with
//
//	go test -run TestDamageSweep ./...
func sweepStride() int {
	if testing.Short() {
		return 7
	}
	return 1
}

// recordBoundaries reports which offsets of a data file sit between records,
// including 0 and the end of the file.
func recordBoundaries(t *testing.T, data []byte) map[int]bool {
	t.Helper()
	at := map[int]bool{0: true}
	for off := 0; off < len(data); {
		_, n, err := decodeRecord(data[off:], defaultMaxKeySize, defaultMaxValueSize)
		if err != nil {
			t.Fatalf("the fixture is not a clean data file: %v at offset %d", err, off)
		}
		off += n
		at[off] = true
	}
	return at
}
