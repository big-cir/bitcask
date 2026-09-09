// Command demo exercises the bitcask package from a real process.
//
// The engine is a library, so this is the smallest thing that can drive it
// end to end and leave a data file behind to inspect:
//
//	go run ./cmd/demo /tmp/bitcask-demo
//	xxd /tmp/bitcask-demo/000000001.data
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"bitcask"
)

func main() {
	log := flag.Bool("hexdump-hint", true, "print the path to hex dump afterwards")
	flag.Parse()

	dir := flag.Arg(0)
	if dir == "" {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <dir>\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if err := os.RemoveAll(dir); err != nil {
		fail(err)
	}

	db, err := bitcask.Open(dir)
	if err != nil {
		fail(err)
	}
	defer db.Close()

	put(db, "k", "first data")
	put(db, "k", "second data!!") // overwrite: appends, leaving the old record
	put(db, "other", "")

	get(db, "k")
	get(db, "other")
	get(db, "missing")

	path := filepath.Join(dir, "000000001.data")
	fi, err := os.Stat(path)
	if err != nil {
		fail(err)
	}
	fmt.Printf("\n%d keys live in %d bytes of log\n", db.Len(), fi.Size())
	if *log {
		fmt.Printf("inspect it with: xxd %s\n", path)
	}
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

func fail(err error) {
	fmt.Fprintln(os.Stderr, "demo:", err)
	os.Exit(1)
}
