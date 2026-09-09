package bitcask

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
