package bitcask

import (
	"fmt"
	"os"
)

// firstFileID is the id given to the data file created in an empty directory.
const firstFileID uint32 = 1

// DB is a handle to an open Bitcask database. It owns the directory passed to
// Open, including every data file inside it.
//
// A DB is not yet safe for concurrent use.
type DB struct {
	dir     string
	opts    options
	keydir  *keydir
	files   map[uint32]*datafile
	active  *datafile
	nextSeq uint64
	closed  bool
}

// Open opens the database rooted at dir, creating the directory if needed.
//
// An existing directory is recovered: every data file is replayed to rebuild
// the index, and a partial record left at the end of the file that was being
// written is truncated away. Damage anywhere else returns ErrCorrupted.
//
// Recovery reads every byte of every data file, so startup is proportional to
// the size of the log rather than to the number of live keys.
func Open(dir string, opts ...Option) (*DB, error) {
	o := defaultOptions()
	for _, apply := range opts {
		apply(&o)
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	db := &DB{
		dir:    dir,
		opts:   o,
		keydir: newKeydir(),
		files:  make(map[uint32]*datafile),
	}

	res, err := recoverKeydir(dir, db.keydir, &o)
	if err != nil {
		return nil, err
	}
	db.nextSeq = res.maxSeq + 1

	if err := db.openFiles(res); err != nil {
		return nil, err
	}
	return db, nil
}

// openFiles opens a handle for every data file the scan reported: read-only
// for the sealed ones, writable for the highest id, which becomes the active
// file again. An empty directory gets its first file created here.
func (db *DB) openFiles(res recoveryResult) error {
	activeID := firstFileID
	if n := len(res.ids); n > 0 {
		activeID = res.ids[n-1]
		for _, id := range res.ids[:n-1] {
			d, err := openDatafileReadOnly(db.dir, id)
			if err != nil {
				db.closeFiles()
				return err
			}
			db.files[id] = d
		}
	}

	active, err := openDatafile(db.dir, activeID)
	if err != nil {
		db.closeFiles()
		return err
	}
	db.files[activeID] = active
	db.active = active

	if res.torn {
		if err := active.truncate(res.tornAt); err != nil {
			db.closeFiles()
			return err
		}
	}
	if len(res.ids) == 0 {
		// The new file's contents are durable once fsynced; its name is only
		// durable once the directory holding that name has been fsynced too.
		if err := syncDir(db.dir); err != nil {
			db.closeFiles()
			return err
		}
	}
	return nil
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
	if err := db.checkSizes(key, value); err != nil {
		return err
	}

	e, err := db.appendRecord(key, value, 0)
	if err != nil {
		return err
	}

	// The index is updated only after the bytes have reached the file. The
	// reverse order would allow an index entry pointing at a record that was
	// never written — harmless while the index cannot outlive a crash, but the
	// habit matters before compaction starts depending on it.
	db.keydir.put(key, e)

	return db.rotateIfFull()
}

// Get returns the value stored under key, or ErrKeyNotFound.
//
// The returned slice is owned by the caller.
//
// Get does not parse the record header: the index already knows the offset and
// length of the value, so the read goes straight to the value bytes. That is
// what makes a lookup cost a single seek, and also why the checksum cannot be
// verified on this path — the header it covers is never read.
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

// Delete removes key, or returns ErrKeyNotFound if it is not present.
//
// Deleting is a write rather than an erasure: a tombstone record is appended
// to the log and the key is dropped from the index. The value it supersedes
// stays on disk until compaction reclaims it, so deleting makes the database
// larger before it makes it smaller.
//
// The tombstone is what stops the value from returning. The index is rebuilt
// by replaying the log, so a deletion recorded only in memory would undo
// itself at the next startup: the original record is still there, and with
// nothing outranking it, the scan would put the value straight back.
func (db *DB) Delete(key []byte) error {
	if db.closed {
		return ErrClosed
	}
	if len(key) == 0 {
		return ErrEmptyKey
	}
	// Nothing is written for a key that is not there. A tombstone for it would
	// be pure garbage: it can never shadow anything.
	if _, ok := db.keydir.get(key); !ok {
		return ErrKeyNotFound
	}

	if _, err := db.appendRecord(key, nil, flagTombstone); err != nil {
		return err
	}
	db.keydir.delete(key)

	return db.rotateIfFull()
}

// Len reports the number of keys currently stored.
func (db *DB) Len() int {
	if db.closed {
		return 0
	}
	return db.keydir.len()
}

// Close flushes buffered writes, forces the active file to disk and closes
// every data file. The in-memory index is discarded; reopening the database
// rebuilds it from the log.
func (db *DB) Close() error {
	if db.closed {
		return ErrClosed
	}
	db.closed = true

	firstErr := db.active.seal()
	if err := db.closeFiles(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// checkSizes rejects an unusable key or an oversized key or value before
// anything is allocated for them. The limits also keep every length that
// reaches the disk inside the range the recovery scan is willing to believe.
func (db *DB) checkSizes(key, value []byte) error {
	switch {
	case len(key) == 0:
		return ErrEmptyKey
	case uint64(len(key)) > uint64(db.opts.maxKeySize):
		return fmt.Errorf("%w: %d bytes, limit %d", ErrKeyTooLarge, len(key), db.opts.maxKeySize)
	case uint64(len(value)) > uint64(db.opts.maxValueSize):
		return fmt.Errorf("%w: %d bytes, limit %d", ErrValueTooLarge, len(value), db.opts.maxValueSize)
	}
	return nil
}

// appendRecord writes one record to the active file and returns the index
// entry that locates its value. It does not touch the index itself.
func (db *DB) appendRecord(key, value []byte, flags uint8) (keydirEntry, error) {
	// The sequence number is consumed even if the write below fails. A failed
	// write can still have put bytes on disk, and handing the same seq out
	// twice would break the rule the whole design rests on: two records with
	// the same seq are two copies of one record.
	seq := db.nextSeq
	db.nextSeq++

	recOffset, err := db.active.append(encodeRecord(seq, key, value, flags))
	if err != nil {
		return keydirEntry{}, err
	}
	return keydirEntry{
		fileID: db.active.id,
		vsz:    uint32(len(value)),
		// The one place that knows how a record's layout maps to a value offset.
		vpos: recOffset + headerSize + int64(len(key)),
		seq:  seq,
	}, nil
}

// rotateIfFull starts a new active file once the current one has grown past
// MaxFileSize.
//
// The bounded file size is the lesser half of what this buys. The other half
// is an invariant: from the moment a file is sealed, it is guaranteed never to
// change again. Compaction will scan sealed files while writes continue, and
// readers will hold descriptors to them without coordinating with the writer —
// both rest on this.
func (db *DB) rotateIfFull() error {
	if db.active.offset <= db.opts.maxFileSize {
		return nil
	}
	return db.rotate()
}

// rotate is ordered so that every failure leaves the database still writable.
// Sealing first would mean that a failure to create the replacement left
// nothing to append to, turning a transient error into a database that has to
// be reopened.
func (db *DB) rotate() error {
	next, err := openDatafile(db.dir, db.active.id+1)
	if err != nil {
		return err
	}
	// Without this, the new file's directory entry is not durable, and a crash
	// can leave records that were written successfully sitting in a file that
	// no longer exists.
	if err := syncDir(db.dir); err != nil {
		next.close()
		return err
	}
	// Sealing flushes the write buffer first. Skipping that would discard
	// whatever the buffer still held, since nothing will write to this file
	// again to push it out.
	if err := db.active.seal(); err != nil {
		next.close()
		return err
	}

	db.files[next.id] = next
	db.active = next
	return nil
}

// closeFiles closes every open data file, reporting the first failure. It is
// also the cleanup path for a partly opened database.
func (db *DB) closeFiles() error {
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
