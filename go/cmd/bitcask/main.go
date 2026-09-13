// Command bitcask is a command-line front end for inspecting and editing a
// database.
//
//	bitcask -d <dir> put <key> <value>
//	bitcask -d <dir> get <key>
//	bitcask -d <dir> del <key>
//	bitcask -d <dir> dump [fileid]
//
// dump is the reason this command exists. It prints every record in a data
// file, in file order, with the offsets and sequence numbers that decide which
// one the index points at — the questions that come up constantly once a
// database spans more than one file.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"bitcask"
)

func main() {
	dir := flag.String("d", "", "database directory")
	maxFileSize := flag.Int64("max-file-size", 0, "rotate the active file past this size (0 = package default)")
	flag.Usage = usage
	flag.Parse()

	if *dir == "" || flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	var opts []bitcask.Option
	if *maxFileSize > 0 {
		opts = append(opts, bitcask.WithMaxFileSize(*maxFileSize))
	}

	args := flag.Args()
	switch cmd := args[0]; cmd {
	case "put":
		need(cmd, args, 3)
		withDB(*dir, opts, func(db *bitcask.DB) error {
			return db.Put([]byte(args[1]), []byte(args[2]))
		})
	case "get":
		need(cmd, args, 2)
		withDB(*dir, opts, func(db *bitcask.DB) error {
			value, err := db.Get([]byte(args[1]))
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(append(value, '\n'))
			return err
		})
	case "del":
		need(cmd, args, 2)
		withDB(*dir, opts, func(db *bitcask.DB) error {
			return db.Delete([]byte(args[1]))
		})
	case "dump":
		// dump reads the files directly. Opening the database would rebuild an
		// index, and an index is the one thing that cannot help here: it holds
		// only the records that won.
		if err := dump(*dir, args[1:]); err != nil {
			fail(err)
		}
	default:
		fmt.Fprintf(os.Stderr, "bitcask: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: bitcask -d <dir> <command> [args]

commands:
  put <key> <value>   store a value
  get <key>           print a value
  del <key>           append a tombstone
  dump [fileid]       print every record in a data file, or in all of them

flags:
  -d <dir>            database directory (required)
  -max-file-size <n>  rotate the active file past this size
`)
}

func need(cmd string, args []string, want int) {
	if len(args) != want {
		fmt.Fprintf(os.Stderr, "bitcask: %s takes %d argument(s)\n", cmd, want-1)
		os.Exit(2)
	}
}

func withDB(dir string, opts []bitcask.Option, fn func(*bitcask.DB) error) {
	db, err := bitcask.Open(dir, opts...)
	if err != nil {
		fail(err)
	}
	if err := fn(db); err != nil {
		db.Close()
		fail(err)
	}
	if err := db.Close(); err != nil {
		fail(err)
	}
}

// Record layout, duplicated here on purpose: this command is a reader of the
// on-disk format from outside the package, which is the only way to notice
// that the format is readable without the package's help.
const (
	headerSize     = 21
	hintHeaderSize = 29
	flagTombstone  = 1
)

func dump(dir string, args []string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.data"))
	if err != nil {
		return err
	}
	sort.Strings(paths)

	if len(args) == 1 {
		id, err := strconv.ParseUint(args[0], 10, 32)
		if err != nil {
			return fmt.Errorf("bitcask: bad file id %q", args[0])
		}
		paths = []string{filepath.Join(dir, fmt.Sprintf("%09d.data", id))}
	}
	if len(paths) == 0 {
		return fmt.Errorf("bitcask: no data files in %s", dir)
	}

	for _, path := range paths {
		if err := dumpDatafile(path); err != nil {
			return err
		}
		hint := strings.TrimSuffix(path, ".data") + ".hint"
		if _, err := os.Stat(hint); err == nil {
			if err := dumpHintfile(hint); err != nil {
				return err
			}
		}
	}
	return nil
}

func dumpDatafile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s  %d bytes\n", filepath.Base(path), len(data))
	fmt.Printf("  %-8s %-6s %-20s %-5s %-6s %s\n", "offset", "len", "seq", "vpos", "vsz", "key")

	var off, live int
	for off < len(data) {
		if off+headerSize > len(data) {
			fmt.Printf("  %-8d incomplete header, %d byte(s) left\n", off, len(data)-off)
			break
		}
		seq := binary.LittleEndian.Uint64(data[off+4 : off+12])
		ksz := int(binary.LittleEndian.Uint32(data[off+12 : off+16]))
		vsz := int(binary.LittleEndian.Uint32(data[off+16 : off+20]))
		flags := data[off+20]
		n := headerSize + ksz + vsz
		if ksz <= 0 || off+n > len(data) {
			fmt.Printf("  %-8d unusable lengths ksz=%d vsz=%d\n", off, ksz, vsz)
			break
		}

		key := data[off+headerSize : off+headerSize+ksz]
		kind := ""
		if flags&flagTombstone != 0 {
			kind = "  TOMBSTONE"
		}
		fmt.Printf("  %-8d %-6d %-20d %-5d %-6d %q%s\n",
			off, n, seq, off+headerSize+ksz, vsz, key, kind)

		off += n
		live++
	}
	fmt.Printf("  %d record(s)\n", live)
	return nil
}

func dumpHintfile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s  %d bytes\n", filepath.Base(path), len(data))

	var off, n int
	for off < len(data) {
		if off+hintHeaderSize > len(data) {
			fmt.Printf("  %-8d incomplete header, %d byte(s) left\n", off, len(data)-off)
			break
		}
		ksz := int(binary.LittleEndian.Uint32(data[off+12 : off+16]))
		if ksz <= 0 || off+hintHeaderSize+ksz > len(data) {
			fmt.Printf("  %-8d unusable ksz=%d\n", off, ksz)
			break
		}
		off += hintHeaderSize + ksz
		n++
	}
	fmt.Printf("  %d record(s) summarised\n", n)
	return nil
}

// fail prints err and exits. Errors from the package already carry a
// "bitcask: " prefix, so nothing is prepended here.
func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
