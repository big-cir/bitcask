// Package bitcask implements the Bitcask storage engine: a log-structured
// hash table for fast key/value data, as described by Sheehy and Smith (2010).
//
// Bitcask keeps every key in an in-memory hash index (the "keydir") that maps
// a key to the exact location of its value on disk. Reads therefore cost one
// hash lookup plus a single disk seek, and writes are appends to a single
// active file — there are no random writes.
//
// The trade-off is deliberate and absolute:
//
//   - Every key must fit in memory. Total value size is bounded only by disk,
//     but the number of keys is bounded by RAM.
//   - There is no ordered iteration. The index is a hash, so range scans are
//     not possible.
//
// This is an embedded engine, not a server. It links into the calling process
// and owns a single directory on disk.
//
// # Usage
//
//	db, err := bitcask.Open("/var/lib/myapp/data")
//	if err != nil {
//		return err
//	}
//	defer db.Close()
//
//	if err := db.Put([]byte("hello"), []byte("world")); err != nil {
//		return err
//	}
//	value, err := db.Get([]byte("hello"))
//
// # On-disk format
//
// A data file is a sequence of records. Each record carries a fixed 21-byte
// header followed by the key and value bytes:
//
//	0        4              12        16        20     21
//	├────────┼──────────────┼─────────┼─────────┼──────┼───────┬─────────┐
//	│ crc32  │     seq      │   ksz   │   vsz   │flags │  key  │  value  │
//	│  (4)   │     (8)      │   (4)   │   (4)   │ (1)  │ (ksz) │  (vsz)  │
//	└────────┴──────────────┴─────────┴─────────┴──────┴───────┴─────────┘
//
// Integers are little-endian. Unlike the original paper, which orders record
// versions by a wall-clock timestamp, this implementation uses a monotonic
// sequence number: wall clocks can move backwards, and compaction reorders
// files, so "highest seq wins" is the only rule that stays correct.
//
// # Durability
//
// Every record carries a CRC-32C over its own bytes, and Open replays the log
// to rebuild the index. A crash leaves a partial record at the end of the file
// that was being written; recovery truncates it and continues, because those
// bytes never returned success to a caller. The same damage inside a sealed
// file has no such explanation and returns ErrCorrupted instead.
//
// Nothing is fsynced per write yet. Data reaches the operating system's page
// cache, which survives the process being killed but not the machine losing
// power.
//
// # Status
//
// This engine is built in stages. The current stage implements append-only
// writes, the in-memory index, checksums, deletes, file rotation, and crash
// recovery. Hint files, compaction, durability policies, and concurrent access
// are not yet implemented.
package bitcask
