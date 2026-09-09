package bitcask

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
)

// datafile is a handle to a single .data file. It treats records as opaque
// byte slices and knows nothing about their contents.
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
// and positions its write offset at the end of the existing content.
func openDatafile(dir string, id uint32) (*datafile, error) {
	path := filepath.Join(dir, datafileName(id))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &datafile{
		id:     id,
		f:      f,
		w:      bufio.NewWriter(f),
		offset: fi.Size(),
	}, nil
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

// close flushes any buffered writes and closes the underlying file.
func (d *datafile) close() error {
	if err := d.w.Flush(); err != nil {
		d.f.Close()
		return err
	}
	return d.f.Close()
}
