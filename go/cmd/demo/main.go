// Command demo exercises the bitcask package from a real process.
//
// The engine is a library, so this is the smallest thing that can drive it end
// to end and leave data files behind to inspect:
//
//	go run ./cmd/demo /tmp/bitcask-demo
//	xxd /tmp/bitcask-demo/000000001.data
//
// It reuses the directory rather than clearing it, so running it twice shows
// the log being recovered. Passing -max-file-size forces rotation:
//
//	go run ./cmd/demo -max-file-size 4096 /tmp/bitcask-demo
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"bitcask"
)

func main() {
	maxFileSize := flag.Int64("max-file-size", 0, "rotate the active file past this size (0 = package default)")
	flag.Parse()

	dir := flag.Arg(0)
	if dir == "" {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <dir>\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}

	var opts []bitcask.Option
	if *maxFileSize > 0 {
		opts = append(opts, bitcask.WithMaxFileSize(*maxFileSize))
	}

	db := open(dir, opts...)

	put(db, "k", "first data")
	put(db, "k", "second data!!") // overwrite: appends, leaving the old record
	put(db, "other", "")

	get(db, "k")
	get(db, "other")
	get(db, "missing")

	del(db, "k")
	get(db, "k") // gone, and the tombstone keeps it gone across restarts

	if err := db.Close(); err != nil {
		fail(err)
	}

	// The index is not stored anywhere. Everything below was rebuilt by
	// replaying the log.
	fmt.Println("\n-- reopened --")
	db = open(dir, opts...)
	get(db, "k")
	get(db, "other")
	if err := db.Close(); err != nil {
		fail(err)
	}

	report(dir)
}

func open(dir string, opts ...bitcask.Option) *bitcask.DB {
	db, err := bitcask.Open(dir, opts...)
	if err != nil {
		fail(err)
	}
	fmt.Printf("open %s: %d keys recovered\n", dir, db.Len())
	return db
}

func put(db *bitcask.DB, key, value string) {
	if err := db.Put([]byte(key), []byte(value)); err != nil {
		fail(err)
	}
	fmt.Printf("put %-8q %q\n", key, value)
}

func get(db *bitcask.DB, key string) {
	value, err := db.Get([]byte(key))
	if err != nil {
		fmt.Printf("get %-8q %v\n", key, err)
		return
	}
	fmt.Printf("get %-8q %q\n", key, value)
}

func del(db *bitcask.DB, key string) {
	err := db.Delete([]byte(key))
	switch {
	case errors.Is(err, bitcask.ErrKeyNotFound):
		fmt.Printf("del %-8q %v\n", key, err)
	case err != nil:
		fail(err)
	default:
		fmt.Printf("del %-8q ok\n", key)
	}
}

// report lists the data files, which is where rotation becomes visible.
func report(dir string) {
	names, err := filepath.Glob(filepath.Join(dir, "*.data"))
	if err != nil {
		fail(err)
	}
	fmt.Println()
	var total int64
	for _, name := range names {
		fi, err := os.Stat(name)
		if err != nil {
			fail(err)
		}
		total += fi.Size()
		fmt.Printf("%s  %7d bytes\n", filepath.Base(name), fi.Size())
	}
	fmt.Printf("\n%d bytes of log in %d file(s)\n", total, len(names))
	if len(names) > 0 {
		fmt.Printf("inspect it with: xxd %s\n", names[0])
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "demo:", err)
	os.Exit(1)
}
