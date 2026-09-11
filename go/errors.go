package bitcask

import "errors"

// Errors returned by DB.
//
// Compare them with errors.Is, never by message: several are returned wrapped
// with the file and offset that produced them, and the wrapping text is not
// part of the API.
var (
	// ErrKeyNotFound is returned when a key is absent from the index. Delete
	// returns it too, rather than writing a tombstone for a key that has none.
	ErrKeyNotFound = errors.New("bitcask: key not found")

	// ErrEmptyKey is returned by Put and Delete when the key has zero length.
	// An empty value is accepted; an empty key is not.
	ErrEmptyKey = errors.New("bitcask: empty key")

	// ErrKeyTooLarge is returned by Put when the key exceeds MaxKeySize.
	ErrKeyTooLarge = errors.New("bitcask: key too large")

	// ErrValueTooLarge is returned by Put when the value exceeds MaxValueSize.
	ErrValueTooLarge = errors.New("bitcask: value too large")

	// ErrCorrupted is returned by Open when a record inside an immutable file
	// fails verification. Damage at the end of the file that was being written
	// is a crash, and is repaired silently; damage anywhere else is not
	// something this package is willing to guess about.
	ErrCorrupted = errors.New("bitcask: corrupted data file")

	// ErrClosed is returned by any operation on a closed database.
	ErrClosed = errors.New("bitcask: database is closed")
)
