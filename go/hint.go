package bitcask

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

// hintHeaderSize is the size of the fixed-length hint record header in bytes.
//
//	0        4        12       16       20      28     29
//	├────────┼────────┼────────┼────────┼───────┼──────┼───────────┐
//	│ crc32c │  seq   │  ksz   │  vsz   │ vpos  │flags │    key    │
//	│  (4)   │  (8)   │  (4)   │  (4)   │  (8)  │ (1)  │  (ksz)    │
//	└────────┴────────┴────────┴────────┴───────┴──────┴───────────┘
//	└──────────── fixed 29 bytes ────────────────────┘
const hintHeaderSize = 29

var errShortHint = errors.New("bitcask: truncated hint record")

// hintRecord is one entry of a hint file: everything the index needs about a
// record except the value bytes.
//
// A hint file is a data file with the value bodies removed and vpos added.
// Removing the values is the point — it is what makes the file small. Adding
// vpos is what that removal costs: a data file scan derives each value offset
// from how far it has read, and a hint file cannot, because its records are no
// longer the same length as the records they describe.
//
// Fields are ordered largest first so the struct carries no padding it does
// not need. That order deliberately differs from the on-disk layout above:
// bytes on disk have no alignment requirement, struct fields do.
//
// There is no ksz field. It is len(key), and a second copy of a length is a
// second thing that can disagree. vsz has to be stored, because the bytes it
// describes are exactly what a hint file leaves out.
type hintRecord struct {
	seq   uint64
	vpos  int64
	vsz   uint32
	flags uint8
	key   []byte
}

// hintName returns the file name of the hint file summarising a data file.
func hintName(id uint32) string { return fmt.Sprintf("%09d.hint", id) }

// encode assembles one hint record, checksum included.
func (h *hintRecord) encode() []byte {
	buf := make([]byte, hintHeaderSize+len(h.key))

	binary.LittleEndian.PutUint64(buf[4:12], h.seq)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(len(h.key)))
	binary.LittleEndian.PutUint32(buf[16:20], h.vsz)
	binary.LittleEndian.PutUint64(buf[20:28], uint64(h.vpos))
	buf[28] = h.flags
	copy(buf[hintHeaderSize:], h.key)

	binary.LittleEndian.PutUint32(buf[0:4], crc32.Checksum(buf[4:], crcTable))

	return buf
}

// decodeHintRecord parses the hint record at the start of src and reports how
// many bytes it consumed. The returned key aliases src.
//
// The checks mirror the ones a data record goes through, and for the same
// reason: these bytes may be a half-written record rather than a record.
func decodeHintRecord(src []byte, maxKeySize uint32) (hintRecord, int, error) {
	if len(src) < hintHeaderSize {
		return hintRecord{}, 0, errShortHint
	}

	ksz := binary.LittleEndian.Uint32(src[12:16])
	// A zero ksz is not merely unusable, it is impossible: Put rejects an
	// empty key, so no hint record was ever written with one.
	if ksz == 0 || ksz > maxKeySize {
		return hintRecord{}, 0, errBadSize
	}
	total := int64(hintHeaderSize) + int64(ksz)
	if total > int64(len(src)) {
		return hintRecord{}, 0, errShortHint
	}

	n := int(total)
	if crc32.Checksum(src[4:n], crcTable) != binary.LittleEndian.Uint32(src[0:4]) {
		return hintRecord{}, 0, errChecksumMismatch
	}

	return hintRecord{
		seq:   binary.LittleEndian.Uint64(src[4:12]),
		vpos:  int64(binary.LittleEndian.Uint64(src[20:28])),
		vsz:   binary.LittleEndian.Uint32(src[16:20]),
		flags: src[28],
		key:   src[hintHeaderSize:n],
	}, n, nil
}

// hintTempName returns the name writeHint assembles a hint file under before
// putting it in place.
func hintTempName(id uint32) string { return hintName(id) + ".tmp" }

// writeHint writes the hint file for a sealed data file, replacing any
// existing one.
//
// The file is assembled under a temporary name and renamed into place. That is
// not tidiness: a hint file has to be all-or-nothing, and appending to its
// final name cannot give that.
//
// Writing in place fails in a way nothing downstream can detect. A crash
// partway through leaves a shorter file, and if it happens to end on a record
// boundary then every record in it parses, the checksums all match, and the
// file is indistinguishable from a complete summary of a smaller data file.
// Recovery would trust it and build an index missing every key after the cut —
// no error, no corruption report, just keys that are on disk and unreachable.
// A rename cannot land halfway, so the only two outcomes are the old hint (or
// none) and the new one.
func writeHint(dir string, id uint32, records []hintRecord) error {
	tmp := filepath.Join(dir, hintTempName(id))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(f)
	for i := range records {
		if _, err := w.Write(records[i].encode()); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	// The contents have to be on disk before the rename publishes the name,
	// or a crash can leave the name pointing at bytes that never arrived.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	if err := os.Rename(tmp, filepath.Join(dir, hintName(id))); err != nil {
		os.Remove(tmp)
		return err
	}
	// The rename itself is a directory change, so the directory needs its own
	// fsync for the new name to survive a power cut.
	return syncDir(dir)
}

// removeStaleHintTemps deletes half-written hint files left behind by a crash.
//
// They are harmless — nothing reads a name ending in .tmp — but a database
// that crashed repeatedly would accumulate them, and a leftover file that
// nothing will ever finish writing is worth removing while the directory is
// already being examined.
func removeStaleHintTemps(dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.hint.tmp"))
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// loadHint reads the hint file summarising a data file.
//
// Any error means the hint cannot be used and the caller has to read the data
// file instead. None of them means the database is damaged: a hint file is
// derived from a data file and can be rebuilt or discarded at will.
//
// A hint that is only partly readable is rejected as a whole. Half a summary
// produces half an index, and there is no way to tell from the hint which keys
// are missing from it — so the partial result is not merely incomplete, it is
// indistinguishable from a complete one.
func loadHint(dir string, id uint32, maxKeySize uint32) ([]hintRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, hintName(id)))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s: %w", hintName(id), errShortHint)
	}

	var records []hintRecord
	for off := 0; off < len(data); {
		h, n, err := decodeHintRecord(data[off:], maxKeySize)
		if err != nil {
			return nil, fmt.Errorf("%s at offset %d: %w", hintName(id), off, err)
		}
		records = append(records, h)
		off += n
	}

	if err := checkHintCoversFile(dir, id, records); err != nil {
		return nil, err
	}
	return records, nil
}

var errIncompleteHint = errors.New("bitcask: hint file does not cover its data file")

// checkHintCoversFile rejects a hint that summarises only part of its data
// file.
//
// Every check up to this point is per record, and per-record checks cannot see
// this failure at all. Cut a hint file at a record boundary and every record
// left in it decodes, every checksum matches, and the result is a valid
// summary — of a file that does not exist. Recovery would build an index
// missing every key past the cut and report nothing, which is worse than any
// corruption it does report: the keys are on disk and unreachable.
//
// The last record of a complete hint describes the last record of the data
// file, so its value ends exactly where the data file ends. A hint that lost
// records points somewhere in the middle instead. One stat call settles it,
// with no bytes of the data file read.
func checkHintCoversFile(dir string, id uint32, records []hintRecord) error {
	fi, err := os.Stat(filepath.Join(dir, datafileName(id)))
	if err != nil {
		return err
	}

	var covered int64
	if n := len(records); n > 0 {
		last := records[n-1]
		covered = last.vpos + int64(last.vsz)
	}
	if covered != fi.Size() {
		return fmt.Errorf("%w: %s covers %d of %d bytes of %s",
			errIncompleteHint, hintName(id), covered, fi.Size(), datafileName(id))
	}
	return nil
}

// buildHint summarises a sealed data file into its hint file.
//
// It re-reads the data file rather than reusing what the write path already
// knew about each record. Re-reading costs one pass over a file that was just
// written and is still in the page cache, and it removes the failure this
// whole task is exposed to: the value offsets in the hint come from the same
// scan the recovery path uses, so the two cannot arrive at different numbers.
//
// A hint is only ever built for a sealed file. A summary of a file that is
// still being appended to is stale the moment it is written.
func buildHint(dir string, id uint32, opts *options) error {
	var records []hintRecord
	scan, err := scanDatafile(filepath.Join(dir, datafileName(id)), opts,
		func(rec record, recOffset int64) {
			records = append(records, hintRecord{
				seq:   rec.seq,
				vpos:  valueOffset(recOffset, rec.key),
				vsz:   uint32(len(rec.value)),
				flags: rec.flags,
				key:   rec.key,
			})
		})
	if err != nil {
		return err
	}
	if scan.torn {
		return fmt.Errorf("%s at offset %d: %w", datafileName(id), scan.lastGood, scan.reason)
	}
	return writeHint(dir, id, records)
}
