package bitcask

import "sync"

// keydirEntry locates a value on disk. It deliberately holds no value bytes:
// the index answers "where is it", never "what is it".
//
// Field order matters. Placing the two uint32 fields first fills eight bytes
// exactly, so the struct is 24 bytes with no padding. Interleaving 4- and
// 8-byte fields pads it out to 32. With a million keys that is 8 MB of pure
// waste, and since the keydir is resident in memory it directly lowers the
// number of keys the engine can hold.
type keydirEntry struct {
	fileID uint32 // which data file holds the record
	vsz    uint32 // value length, i.e. how many bytes to read
	vpos   int64  // offset of the value body; int64 to match (*os.File).ReadAt
	seq    uint64 // version of this entry, used by recovery and compaction
}

// keydir is the in-memory index mapping keys to value locations. It has no
// knowledge of the files themselves.
//
// The mutex is a read-write lock because reads dominate and RLock holders do
// not block each other.
type keydir struct {
	mu sync.RWMutex
	m  map[string]keydirEntry
}

func newKeydir() *keydir {
	return &keydir{m: make(map[string]keydirEntry)}
}

// get returns the entry for key, if present.
//
// The inline m[string(key)] conversion is recognized by the compiler and does
// not allocate. Hoisting it into a variable first would copy the key.
func (k *keydir) get(key []byte) (keydirEntry, bool) {
	k.mu.RLock()
	e, ok := k.m[string(key)]
	k.mu.RUnlock()
	return e, ok
}

// put associates key with e, replacing any existing entry.
//
// Storing the key does copy it, which is necessary: the caller may reuse or
// mutate the slice it passed in, and the map must not alias that memory.
func (k *keydir) put(key []byte, e keydirEntry) {
	k.mu.Lock()
	k.m[string(key)] = e
	k.mu.Unlock()
}

// delete removes key from the index. A deleted key is absent from the keydir;
// its tombstone lives only on disk.
func (k *keydir) delete(key []byte) {
	k.mu.Lock()
	delete(k.m, string(key))
	k.mu.Unlock()
}

// len reports the number of live keys.
func (k *keydir) len() int {
	k.mu.RLock()
	n := len(k.m)
	k.mu.RUnlock()
	return n
}
