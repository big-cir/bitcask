package bitcask

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// Measurement tests report numbers rather than asserting them. They exist so
// the figures recorded in the learning log can be reproduced on demand:
//
//	go test -run TestMeasure -v .
//
// Header overhead as a share of value size, measured from real file sizes.
func TestMeasureOverhead(t *testing.T) {
	const keyLen = 8
	for _, vsz := range []int{10, 100, 1024, 102400} {
		dir := t.TempDir()
		db, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		key := make([]byte, keyLen)
		copy(key, fmt.Sprintf("%08d", 1))
		if err := db.Put(key, make([]byte, vsz)); err != nil {
			t.Fatal(err)
		}
		db.Close()

		fi, err := os.Stat(filepath.Join(dir, datafileName(firstFileID)))
		if err != nil {
			t.Fatal(err)
		}
		over := float64(int(fi.Size())-vsz) / float64(vsz) * 100
		t.Logf("value=%7dB  record=%7dB  overhead=%8.3f%%", vsz, fi.Size(), over)
	}
}

// Size of one index entry, and what the same fields cost when reordered so
// the compiler must pad them.
func TestMeasureEntrySize(t *testing.T) {
	type reordered struct {
		fileID uint32
		vpos   int64
		vsz    uint32
		seq    uint64
	}
	t.Logf("keydirEntry=%dB  reordered=%dB", unsafe.Sizeof(keydirEntry{}), unsafe.Sizeof(reordered{}))
}

// Resident cost of the index, which is what bounds how many keys the engine
// can hold. Skipped under -short because a million inserts is not free.
func TestMeasureKeydirRAM(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~100MB")
	}
	for _, n := range []int{100_000, 1_000_000} {
		kd := newKeydir()
		key := make([]byte, 16)

		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		for i := 0; i < n; i++ {
			copy(key, fmt.Sprintf("%016d", i))
			kd.put(key, keydirEntry{fileID: 1, vsz: 100, vpos: int64(i) * 129, seq: uint64(i)})
		}

		runtime.GC()
		runtime.ReadMemStats(&after)
		delta := after.HeapAlloc - before.HeapAlloc
		t.Logf("keys=%9d  heap=%6.1fMB  per-key=%3dB  (extrapolated 100M: %5.1fGB)",
			n, float64(delta)/(1<<20), delta/uint64(n),
			float64(delta/uint64(n))*100e6/(1<<30))
		if kd.len() != n {
			t.Fatal("keydir len mismatch")
		}
	}
}

// Startup cost with and without hint files, across value sizes.
//
// This is the measurement v2 exists to produce. Recovery reads either every
// byte of every data file, or the keys alone — so the scan path costs what the
// log weighs and the hint path costs what the key count is. Holding the key
// count fixed and growing the values separates the two.
func TestMeasureStartup(t *testing.T) {
	if testing.Short() {
		t.Skip("writes several hundred MB")
	}

	const (
		keys  = 20_000
		files = 8 // how many data files each row is split into
	)

	t.Logf("%d keys, each row rotated into about %d data files", keys, files)
	t.Logf("%-8s %6s %10s %10s %9s %9s %10s %8s",
		"value", "files", "data", "hint", "data/hint", "w/ hint", "w/o hint", "speedup")

	for _, valueSize := range []int{16, 64, 256, 1024, 4096, 16384} {
		dir := t.TempDir()

		// MaxFileSize is scaled per row. A fixed threshold would leave the
		// small-value rows in a single file, which never rotates and so never
		// gets a hint at all -- the comparison would then be measuring nothing.
		recordSize := int64(headerSize + len("key_00000000") + valueSize)
		opts := []Option{WithMaxFileSize(recordSize * keys / files)}

		db, err := Open(dir, opts...)
		if err != nil {
			t.Fatal(err)
		}
		value := make([]byte, valueSize)
		for i := 0; i < keys; i++ {
			if err := db.Put([]byte(fmt.Sprintf("key_%08d", i)), value); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		dataBytes, dataFiles := totalSize(t, dir, "*.data")
		hintBytes, hintFiles := totalSize(t, dir, "*.hint")
		if hintFiles == 0 {
			t.Fatalf("value=%d produced no hint files; nothing to compare", valueSize)
		}

		withHints := timeOpen(t, dir, opts)
		for _, p := range glob(t, dir, "*.hint") {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		withoutHints := timeOpen(t, dir, opts)

		t.Logf("%-8d %6d %9.1fM %9.2fM %8.1fx %8.1fms %9.1fms %7.1fx",
			valueSize, dataFiles,
			float64(dataBytes)/(1<<20), float64(hintBytes)/(1<<20),
			float64(dataBytes)/float64(hintBytes),
			float64(withHints.Microseconds())/1000,
			float64(withoutHints.Microseconds())/1000,
			float64(withoutHints)/float64(withHints))
	}

	t.Log("")
	t.Log("The hint column barely moves: it is the key count, not the data size.")
	t.Log("Times are page-cache reads, since the files were just written. On a")
	t.Log("cold cache the scan column is disk-bound and the gap is wider.")

	// The data/hint byte ratio above reaches 457x while the time ratio stops
	// near 6x. The active file is why: it has no hint and is always read in
	// full, so it sets a ceiling on what hints can save. Splitting the same
	// data into more files shrinks the active file's share and raises it.
	t.Log("")
	t.Logf("%d keys x 4096 B, varying how many files the data is split into:", keys)
	t.Logf("%-7s %11s %11s %9s %10s %8s",
		"files", "active", "active/all", "w/ hint", "w/o hint", "speedup")

	const valueSize = 4096
	recordSize := int64(headerSize + len("key_00000000") + valueSize)
	for _, files := range []int{2, 8, 32, 128} {
		dir := t.TempDir()
		opts := []Option{WithMaxFileSize(recordSize * keys / int64(files))}

		db, err := Open(dir, opts...)
		if err != nil {
			t.Fatal(err)
		}
		value := make([]byte, valueSize)
		for i := 0; i < keys; i++ {
			if err := db.Put([]byte(fmt.Sprintf("key_%08d", i)), value); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		dataBytes, _ := totalSize(t, dir, "*.data")
		ids, err := datafileIDs(dir)
		if err != nil {
			t.Fatal(err)
		}
		activeBytes := fileSize(t, filepath.Join(dir, datafileName(ids[len(ids)-1])))

		withHints := timeOpen(t, dir, opts)
		for _, p := range glob(t, dir, "*.hint") {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		withoutHints := timeOpen(t, dir, opts)

		t.Logf("%-7d %10.1fM %10.1f%% %8.1fms %9.1fms %7.1fx",
			len(ids),
			float64(activeBytes)/(1<<20),
			100*float64(activeBytes)/float64(dataBytes),
			float64(withHints.Microseconds())/1000,
			float64(withoutHints.Microseconds())/1000,
			float64(withoutHints)/float64(withHints))
	}
}

func timeOpen(t *testing.T, dir string, opts []Option) time.Duration {
	t.Helper()
	start := time.Now()
	db, err := Open(dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return elapsed
}

func glob(t *testing.T, dir, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func totalSize(t *testing.T, dir, pattern string) (int64, int) {
	t.Helper()
	paths := glob(t, dir, pattern)
	var total int64
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
	}
	return total, len(paths)
}
