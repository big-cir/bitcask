package bitcask

// An annotated walk through one database, printing the actual bytes at every
// step:
//
//	go test -run TestWalkthrough -v .
//
// This asserts almost nothing. It is where the byte layouts, checksum values
// and timings quoted in the notes come from, and running it is how those
// numbers get checked again after the format changes. Every other test in this
// package decides whether the code is right; this one shows what it does.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func section(n int, title string) {
	fmt.Printf("\n%s\n== %d. %s\n%s\n", strings.Repeat("=", 78), n, title, strings.Repeat("=", 78))
}

func hex(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(parts, " ")
}

// dumpRecord prints one record's bytes, field by field.
func dumpRecord(rec []byte, fileOffset int64) {
	h, _ := decodeHeader(rec)
	key := rec[headerSize : headerSize+int(h.ksz)]
	val := rec[headerSize+int(h.ksz):]

	fmt.Printf("  %-8s %-26s %s\n", "offset", "bytes", "field")
	row := func(off int, b []byte, field string) {
		fmt.Printf("  %-8d %-26s %s\n", int(fileOffset)+off, hex(b), field)
	}
	row(0, rec[0:4], fmt.Sprintf("crc32 = 0x%08x", h.crc))
	row(4, rec[4:12], fmt.Sprintf("seq   = %d", h.seq))
	row(12, rec[12:16], fmt.Sprintf("ksz   = %d", h.ksz))
	row(16, rec[16:20], fmt.Sprintf("vsz   = %d", h.vsz))
	flag := "0x00 (normal)"
	if h.flags&flagTombstone != 0 {
		flag = "0x01 (TOMBSTONE)"
	}
	row(20, rec[20:21], "flags = "+flag)
	row(headerSize, key, fmt.Sprintf("key   = %q", key))
	if len(val) > 0 {
		row(headerSize+int(h.ksz), val, fmt.Sprintf("value = %q", val))
	} else {
		fmt.Printf("  %-8s %-26s %s\n", "-", "(none)", "value = <no bytes at all>")
	}
}

func dumpKeydir(kd *keydir) {
	kd.mu.RLock()
	keys := make([]string, 0, len(kd.m))
	for k := range kd.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("  keydir (in RAM, %d entries):\n", len(keys))
	for _, k := range keys {
		e := kd.m[k]
		fmt.Printf("    %-10q -> fileID=%d  vpos=%-4d vsz=%-3d seq=%d\n", k, e.fileID, e.vpos, e.vsz, e.seq)
	}
	if len(keys) == 0 {
		fmt.Println("    (empty)")
	}
	kd.mu.RUnlock()
}

func TestWalkthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("prints about 150 lines and measures fsync")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, datafileName(firstFileID))

	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// ---------------------------------------------------------------
	section(1, `WRITE: Put("user:1", "alice")`)
	fmt.Println(`This is what encodeRecord() builds and hands to write(2).`)
	fmt.Println()
	rec := encodeRecord(1, []byte("user:1"), []byte("alice"), 0)
	dumpRecord(rec, 0)
	fmt.Printf("\n  total = %d header + %d key + %d value = %d bytes\n", headerSize, 6, 5, len(rec))

	if err := db.Put([]byte("user:1"), []byte("alice")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	fmt.Printf("\n  %s is now %d bytes\n\n", datafileName(firstFileID), fi.Size())
	dumpKeydir(db.keydir)

	// ---------------------------------------------------------------
	section(2, `READ: Get("user:1")`)
	e, _ := db.keydir.get([]byte("user:1"))
	fmt.Printf("  step 1  hash lookup in RAM   -> vpos=%d vsz=%d\n", e.vpos, e.vsz)
	fmt.Printf("  step 2  pread(fd, buf[%d], offset=%d)\n", e.vsz, e.vpos)
	got, _ := db.Get([]byte("user:1"))
	fmt.Printf("  step 3  got %q\n", got)
	fmt.Println()
	fmt.Println("  Note what was NOT read: the 21-byte header, and every other record.")
	fmt.Println("  One hash lookup + one pread. That is the whole read path.")

	// ---------------------------------------------------------------
	section(3, `OVERWRITE: Put("user:1", "ALICE-v2")`)
	if err := db.Put([]byte("user:1"), []byte("ALICE-v2")); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(path)
	fmt.Printf("  file grew to %d bytes -- nothing was overwritten in place\n\n", fi.Size())
	dumpKeydir(db.keydir)
	fmt.Println()
	fmt.Println(`  The old "alice" bytes are still at offset 27. Unreachable, but present.`)
	fmt.Println("  That is the garbage compaction will later reclaim.")

	// ---------------------------------------------------------------
	section(4, `DELETE: Delete("user:1")`)
	if err := db.Put([]byte("keep"), []byte("survivor")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := db.Delete([]byte("user:1")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	fmt.Printf("  file went from %d to %d bytes: deleting ADDED %d bytes\n\n", before.Size(), after.Size(), after.Size()-before.Size())

	tomb := encodeRecord(4, []byte("user:1"), nil, flagTombstone)
	dumpRecord(tomb, before.Size())
	fmt.Println()
	dumpKeydir(db.keydir)
	fmt.Println()
	fmt.Println(`  "user:1" is gone from RAM. Both of its value records are still on disk.`)
	fmt.Println("  The tombstone is the only thing that will stop them coming back.")

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// ---------------------------------------------------------------
	section(5, "THE WHOLE FILE, record by record")
	data, _ := os.ReadFile(path)
	var off int64
	for off < int64(len(data)) {
		r, n, err := decodeRecord(data[off:], defaultMaxKeySize, defaultMaxValueSize)
		if err != nil {
			fmt.Printf("  offset %-4d STOP: %v\n", off, err)
			break
		}
		kind := "value"
		if r.isTombstone() {
			kind = "TOMBSTONE"
		}
		fmt.Printf("  offset %-4d len %-4d seq %-3d %-10s key=%-10q value=%q\n",
			off, n, r.seq, kind, r.key, r.value)
		off += int64(n)
	}
	fmt.Printf("\n  %d bytes total, of which 1 key (%q) is live.\n", len(data), "keep")

	// ---------------------------------------------------------------
	section(6, "CRC: what it is and what it catches")
	fmt.Println("  A CRC is a 4-byte number computed from bytes. Same bytes -> same number.")
	fmt.Println("  One byte different -> almost certainly a different number.")
	fmt.Println()
	sample := encodeRecord(1, []byte("user:1"), []byte("alice"), 0)
	fmt.Printf("  stored crc                = 0x%08x\n", binary.LittleEndian.Uint32(sample[0:4]))
	fmt.Printf("  crc32c(bytes 4..end)      = 0x%08x   <- matches, so the record is intact\n",
		crc32.Checksum(sample[4:], crcTable))
	fmt.Println()
	fmt.Println("  Now flip one bit inside the VALUE (offset 27, 'a' of \"alice\"):")
	damaged := append([]byte{}, sample...)
	damaged[27] ^= 0x01
	fmt.Printf("    value is now %q\n", damaged[headerSize+6:])
	fmt.Printf("    stored crc                = 0x%08x\n", binary.LittleEndian.Uint32(damaged[0:4]))
	fmt.Printf("    crc32c(bytes 4..end)      = 0x%08x   <- DIFFERENT\n", crc32.Checksum(damaged[4:], crcTable))
	_, _, err = decodeRecord(damaged, defaultMaxKeySize, defaultMaxValueSize)
	fmt.Printf("    decodeRecord -> %v\n", err)
	fmt.Println()
	fmt.Println("  This is why the crc covers bytes 4..end and not just the header:")
	fmt.Println("  the header above is untouched, so a header-only crc would still")
	fmt.Println("  match. The record would be accepted and the damaged value handed")
	fmt.Println("  to the caller, with no error anywhere.")

	// ---------------------------------------------------------------
	section(7, "fsync: the three layers, and what each one costs")
	fmt.Println("  bufio.Writer.Write  -> Go process memory   (dies with the process)")
	fmt.Println("  os.File.Write       -> kernel page cache    (survives kill -9)")
	fmt.Println("  os.File.Sync        -> physical disk        (survives power loss)")
	fmt.Println()

	const n = 100
	measure := func(label string, sync bool) time.Duration {
		mdb, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer mdb.Close()

		val := make([]byte, 100)
		start := time.Now()
		for i := 0; i < n; i++ {
			if err := mdb.Put([]byte(fmt.Sprintf("key%05d", i)), val); err != nil {
				t.Fatal(err)
			}
			if sync {
				if err := mdb.active.f.Sync(); err != nil {
					t.Fatal(err)
				}
			}
		}
		el := time.Since(start)
		fmt.Printf("  %-34s %8.2f ms for %d puts  (%8.0f puts/sec)\n",
			label, float64(el.Microseconds())/1000, n, float64(n)/el.Seconds())
		return el
	}
	noSync := measure("write only (what v1 does now)", false)
	withSync := measure("write + fsync on every put", true)
	fmt.Printf("\n  fsync makes it %.0fx slower. That is the price of surviving a power cut.\n",
		float64(withSync)/float64(noSync))
	fmt.Println("  v1 pays it only on rotate/Close. v4 will make it a choice.")

	// ---------------------------------------------------------------
	section(8, "RECOVERY: rebuilding the index from the log alone")
	fmt.Println("  Nothing about the keydir was ever written to disk. On Open, this runs:")
	fmt.Println()
	kd := newKeydir()
	var maxSeq uint64
	off = 0
	for off < int64(len(data)) {
		r, sz, err := decodeRecord(data[off:], defaultMaxKeySize, defaultMaxValueSize)
		if err != nil {
			break
		}
		if r.seq > maxSeq {
			maxSeq = r.seq
		}
		existing, ok := kd.get(r.key)
		decision := ""
		switch {
		case ok && existing.seq >= r.seq:
			decision = fmt.Sprintf("SKIP  (keydir has seq %d >= %d)", existing.seq, r.seq)
		case r.isTombstone():
			kd.delete(r.key)
			decision = "DELETE from keydir"
		default:
			kd.put(r.key, keydirEntry{fileID: 1, vsz: uint32(len(r.value)), vpos: off + headerSize + int64(len(r.key)), seq: r.seq})
			decision = fmt.Sprintf("PUT   vpos=%d vsz=%d", off+headerSize+int64(len(r.key)), len(r.value))
		}
		fmt.Printf("  offset %-4d seq %-3d key=%-10q -> %s\n", off, r.seq, r.key, decision)
		off += int64(sz)
	}
	fmt.Println()
	dumpKeydir(kd)
	fmt.Printf("\n  nextSeq = maxSeq + 1 = %d\n", maxSeq+1)
	fmt.Println("  If this restarted at 1, the next write of a key would TIE with an old")
	fmt.Println("  record and lose the >= comparison above. Stale value, no error.")

	// ---------------------------------------------------------------
	section(9, "CRASH: a half-written record at the end of the file")
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("this is a torn record"))
	f.Close()
	torn, _ := os.Stat(path)
	fmt.Printf("  appended 21 bytes of garbage: file is now %d bytes (was %d)\n\n", torn.Size(), len(data))

	fmt.Println("  The scan reaches that offset and runs four checks in order:")
	garbage := []byte("this is a torn record")
	gh, _ := decodeHeader(garbage)
	fmt.Printf("    1. enough bytes for a 21-byte header?   yes (%d)\n", len(garbage))
	fmt.Printf("    2. ksz=%d vsz=%d within the limits (%d / %d)?  NO -> stop here\n",
		gh.ksz, gh.vsz, defaultMaxKeySize, defaultMaxValueSize)
	fmt.Printf("       (check 2 comes BEFORE any allocation. make([]byte, %d) would be %.1f GB)\n",
		gh.vsz, float64(gh.vsz)/(1<<30))
	fmt.Println("    3. is 21+ksz+vsz present in the file?   not reached")
	fmt.Println("    4. does the crc match?                  not reached")
	fmt.Println()

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	fixed, _ := os.Stat(path)
	fmt.Printf("  Open() -> no error. file truncated back to %d bytes.\n", fixed.Size())
	v, _ := db2.Get([]byte("keep"))
	fmt.Printf("  Get(\"keep\") -> %q\n", v)
	if _, err := db2.Get([]byte("user:1")); err != nil {
		fmt.Printf("  Get(\"user:1\") -> %v   (the tombstone held)\n", err)
	}
	db2.Close()

	fmt.Print("\n\nto walk a database of your own the same way:\n" +
		"  go run ./cmd/bitcask -d <dir> dump\n\n")
}
