package bitcask

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// firstFileID is the id assigned to the data file created by Open.
const firstFileID uint32 = 1

// Errors returned by DB. Compare them with errors.Is.
var (
	// ErrKeyNotFound is returned when a key is absent from the index.
	ErrKeyNotFound = errors.New("bitcask: key not found")

	// ErrEmptyKey is returned by Put when the key has zero length. An empty
	// value is accepted; an empty key is not.
	ErrEmptyKey = errors.New("bitcask: empty key")

	// ErrClosed is returned by any operation on a closed database.
	ErrClosed = errors.New("bitcask: database is closed")

	// ErrExistingData is returned by Open when the directory already holds
	// data files. Rebuilding the index from an existing log is not yet
	// implemented, and silently ignoring the files would hide the fact.
	ErrExistingData = errors.New("bitcask: directory already contains data files")
)

// DB is a handle to an open Bitcask database. It owns the directory passed to
// Open, including every data file inside it.
//
// A DB is not yet safe for concurrent use.
type DB struct {
	dir     string
	keydir  *keydir
	files   map[uint32]*datafile
	active  *datafile
	nextSeq uint64
	closed  bool
}

// Open opens the database rooted at dir, creating the directory if needed.
//
// It returns ErrExistingData if dir already contains data files.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	existing, err := filepath.Glob(filepath.Join(dir, "*.data"))
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return nil, fmt.Errorf("%w: %v", ErrExistingData, existing)
	}

	active, err := openDatafile(dir, firstFileID)
	if err != nil {
		return nil, err
	}
	return &DB{
		dir:     dir,
		keydir:  newKeydir(),
		files:   map[uint32]*datafile{firstFileID: active},
		active:  active,
		nextSeq: 1, // zero is reserved to mean "no sequence"
	}, nil
}

// Put stores value under key, overwriting any previous value.
//
// Overwriting appends a new record and repoints the index at it. The
// superseded record stays on disk as garbage until compaction reclaims it.
//
// An empty value is stored as such. An empty key returns ErrEmptyKey.
func (db *DB) Put(key, value []byte) error {
	if db.closed {
		return ErrClosed
	}
	if len(key) == 0 {
		return ErrEmptyKey
	}

	seq := db.nextSeq
	db.nextSeq++

	rec := encodeRecord(seq, key, value, 0)

	recOffset, err := db.active.append(rec)
	if err != nil {
		return err
	}

	// The one place that knows how a record's layout maps to a value offset.
	vpos := recOffset + headerSize + int64(len(key))

	db.keydir.put(key, keydirEntry{
		fileID: db.active.id,
		vsz:    uint32(len(value)),
		vpos:   vpos,
		seq:    seq,
	})
	return nil
}

// Get returns the value stored under key, or ErrKeyNotFound.
//
// The returned slice is owned by the caller.
//
// Get does not parse the record header: the index already knows the offset and
// length of the value, so the read goes straight to the value bytes. That is
// what makes a lookup cost a single seek, and also why the checksum cannot be
// verified on this path.
func (db *DB) Get(key []byte) ([]byte, error) {
	if db.closed {
		return nil, ErrClosed
	}
	e, ok := db.keydir.get(key)
	if !ok {
		return nil, ErrKeyNotFound // no disk access at all
	}
	d, ok := db.files[e.fileID]
	if !ok {
		return nil, fmt.Errorf("bitcask: index refers to unknown file id %d", e.fileID)
	}
	buf := make([]byte, e.vsz)
	if e.vsz == 0 {
		return buf, nil
	}
	if err := d.readAt(buf, e.vpos); err != nil {
		return nil, fmt.Errorf("bitcask: read value at %s:%d: %w",
			datafileName(e.fileID), e.vpos, err)
	}
	return buf, nil
}

// Len reports the number of keys currently stored.
func (db *DB) Len() int {
	if db.closed {
		return 0
	}
	return db.keydir.len()
}

// Close flushes buffered writes and closes every data file. The in-memory
// index is discarded; reopening the database rebuilds it from the log.
func (db *DB) Close() error {
	if db.closed {
		return ErrClosed
	}
	db.closed = true

	var firstErr error
	for _, d := range db.files {
		if err := d.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	db.files = nil
	db.active = nil
	return firstErr
}
