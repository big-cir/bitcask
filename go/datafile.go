package bitcask

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// errImmutable guards the invariant that only the active file is ever written
// to. It should never reach a caller of the package; if it does, the write
// path has lost track of which file is active.
var errImmutable = errors.New("bitcask: write to an immutable data file")

// datafile is a handle to a single .data file. It treats records as opaque
// byte slices and knows nothing about their contents.
//
// A nil w marks the file as immutable. Exactly one file in a database — the
// active one — has a writer; every other file is sealed and read-only for the
// rest of its existence. That invariant is what lets compaction scan old files
// while writes continue, and what will let readers work without a lock.
type datafile struct {
	id     uint32
	f      *os.File
	w      *bufio.Writer
	offset int64
}

// datafileName returns the file name for a data file id. The zero-padded
// width makes lexical order match numeric order, so a sorted glob is enough.
func datafileName(id uint32) string { return fmt.Sprintf("%09d.data", id) }

// openDatafile opens (creating if necessary) the data file with the given id
// for appending and positions its write offset at the end of the existing
// content. The result is writable: it is the active file.
func openDatafile(dir string, id uint32) (*datafile, error) {
	d, err := openFile(dir, id, os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	d.w = bufio.NewWriter(d.f)
	return d, nil
}

// openDatafileReadOnly opens an existing data file for reading only. Sealed
// files are opened this way so that the operating system, and not just this
// package's own discipline, enforces that they never change.
func openDatafileReadOnly(dir string, id uint32) (*datafile, error) {
	return openFile(dir, id, os.O_RDONLY)
}

func openFile(dir string, id uint32, flag int) (*datafile, error) {
	f, err := os.OpenFile(filepath.Join(dir, datafileName(id)), flag, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &datafile{id: id, f: f, offset: fi.Size()}, nil
}

// append writes rec to the end of the file and returns the offset at which
// the record starts. Callers derive the value offset by adding headerSize and
// the key length.
//
// The write buffer is flushed on every call so that a value can be read back
// immediately through readAt. Tracking a separate flushed offset would avoid
// the syscall per write, at the cost of a read path that must know how far the
// buffer has drained.
func (d *datafile) append(rec []byte) (int64, error) {
	if d.w == nil {
		return 0, errImmutable
	}
	start := d.offset
	if _, err := d.w.Write(rec); err != nil {
		return 0, err
	}
	if err := d.w.Flush(); err != nil {
		return 0, err
	}
	d.offset += int64(len(rec))
	return start, nil
}

// readAt fills buf from the given offset.
//
// It uses ReadAt, which maps to pread(2) and leaves the file offset untouched,
// so a single *os.File can serve concurrent readers. A Seek plus Read pair
// would let two goroutines interleave and silently read each other's data.
func (d *datafile) readAt(buf []byte, off int64) error {
	_, err := d.f.ReadAt(buf, off)
	return err
}

// seal flushes the write buffer, forces the file's contents to the physical
// disk and marks the file immutable. It stays open, because readers still need
// it; it just stops being writable.
//
// Sealing is idempotent, so Close can call it without knowing whether a
// rotation already did.
//
// The file is marked immutable only once the fsync has succeeded. Marking it
// first would turn a failed fsync into a file that is no longer writable and
// not yet durable, and every later append would be rejected for the wrong
// reason.
func (d *datafile) seal() error {
	if d.w == nil {
		return nil
	}
	if err := d.w.Flush(); err != nil {
		return err
	}
	if err := d.f.Sync(); err != nil {
		return err
	}
	d.w = nil
	return nil
}

// truncate cuts the file back to size and forces the new length to disk.
//
// The bytes it removes are writes that never returned success to a caller, so
// discarding them breaks no promise. Keeping them would be far worse: a log is
// only parseable front to back, so a later append landing on top of a
// half-written record would put itself, and everything after it, permanently
// out of reach.
func (d *datafile) truncate(size int64) error {
	if err := d.f.Truncate(size); err != nil {
		return err
	}
	d.offset = size
	// A length is metadata, and metadata needs its own fsync to survive a
	// power cut just as much as the contents do.
	return d.f.Sync()
}

// close flushes any buffered writes and closes the underlying file.
func (d *datafile) close() error {
	if d.w != nil {
		if err := d.w.Flush(); err != nil {
			d.f.Close()
			return err
		}
	}
	return d.f.Close()
}

// syncDir forces a directory's entries to disk.
//
// Fsyncing a file makes its contents durable. It says nothing about whether
// the file still exists after a crash: the name lives in the directory, and
// the directory is a file of its own that has not been synced. Skipping this
// is the standard way to lose a file that was successfully written.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// datafileIDs returns the ids of the data files in dir, ascending.
//
// Names that do not round-trip through datafileName are skipped rather than
// rejected. A stray file in the directory was not written by this package, and
// refusing to open the database over it would be a worse outcome than leaving
// it alone.
func datafileIDs(dir string) ([]uint32, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.data"))
	if err != nil {
		return nil, err
	}
	ids := make([]uint32, 0, len(names))
	for _, name := range names {
		base := filepath.Base(name)
		id, err := strconv.ParseUint(strings.TrimSuffix(base, ".data"), 10, 32)
		if err != nil || datafileName(uint32(id)) != base {
			continue
		}
		ids = append(ids, uint32(id))
	}
	slices.Sort(ids)
	return ids, nil
}
