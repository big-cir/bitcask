package bitcask

import "fmt"

// Defaults for the size limits.
//
// MaxKeySize and MaxValueSize are not conveniences. They are the only bound
// the recovery scan has on a length field it read out of a possibly corrupt
// header, and the reason it can reject a ksz of 0xFFFFFFFF instead of trying
// to allocate four gigabytes for it.
const (
	defaultMaxFileSize  int64  = 64 << 20 // 64 MiB
	defaultMaxKeySize   uint32 = 64 << 10 // 64 KiB
	defaultMaxValueSize uint32 = 16 << 20 // 16 MiB
)

// options holds the settings a database was opened with.
//
// It stays unexported and is built through Option functions so that later
// stages can add settings without changing the signature of Open.
type options struct {
	maxFileSize  int64
	maxKeySize   uint32
	maxValueSize uint32
}

func defaultOptions() options {
	return options{
		maxFileSize:  defaultMaxFileSize,
		maxKeySize:   defaultMaxKeySize,
		maxValueSize: defaultMaxValueSize,
	}
}

// validate rejects settings that would make the engine misbehave in ways that
// are hard to trace back to the call to Open. A zero MaxKeySize, for instance,
// would make the recovery scan reject every record as corrupt.
func (o *options) validate() error {
	switch {
	case o.maxFileSize <= 0:
		return fmt.Errorf("bitcask: MaxFileSize must be positive, got %d", o.maxFileSize)
	case o.maxKeySize == 0:
		return fmt.Errorf("bitcask: MaxKeySize must be positive")
	case o.maxValueSize == 0:
		return fmt.Errorf("bitcask: MaxValueSize must be positive")
	}
	return nil
}

// An Option configures a database at Open time.
type Option func(*options)

// WithMaxFileSize sets the size past which the active file is rotated and a
// new one becomes active. The default is 64 MiB.
//
// A small value is useful in tests: it is the only way to get more than one
// data file without writing 64 MiB.
func WithMaxFileSize(n int64) Option {
	return func(o *options) { o.maxFileSize = n }
}

// WithMaxKeySize sets the largest key the database will accept, and the
// largest ksz the recovery scan will believe. The default is 64 KiB.
func WithMaxKeySize(n uint32) Option {
	return func(o *options) { o.maxKeySize = n }
}

// WithMaxValueSize sets the largest value the database will accept, and the
// largest vsz the recovery scan will believe. The default is 16 MiB.
func WithMaxValueSize(n uint32) Option {
	return func(o *options) { o.maxValueSize = n }
}
