package bitcask

import (
	"fmt"
	"os"
	"path/filepath"
)

// recoveryResult is what a scan of a directory's data files learned.
type recoveryResult struct {
	// ids holds every data file id found, ascending. The last one is the file
	// that was active when the database was last open, and is the only file
	// that may be written to again.
	ids []uint32

	// maxSeq is the highest sequence number anywhere in the log. Writing
	// continues from maxSeq+1; restarting the counter at 1 would make new
	// writes lose to old ones, since the whole ordering rule is "highest seq
	// wins".
	maxSeq uint64

	// torn records that the active file ends in a partial record, and at which
	// offset. The caller truncates there.
	torn   bool
	tornAt int64
}

// recoverKeydir rebuilds kd by replaying every data file in dir.
//
// This is the property that makes a log-structured store possible: the log is
// the database, and the index is a cache of it that can be thrown away and
// derived again. Nothing about the in-memory state is persisted, which is why
// there is no scenario where the index and the data disagree.
//
// Files are read in file id order, but file order is not what decides which
// version of a key wins — the highest seq does. Both rules give the same answer
// today, which is exactly why implementing the wrong one here is dangerous:
// the tests pass, and the bug appears later, when compaction starts writing
// old records into new files.
func recoverKeydir(dir string, kd *keydir, opts *options) (recoveryResult, error) {
	ids, err := datafileIDs(dir)
	if err != nil {
		return recoveryResult{}, err
	}

	res := recoveryResult{ids: ids}
	for i, id := range ids {
		scan, err := scanDatafile(filepath.Join(dir, datafileName(id)), id, kd, opts)
		if err != nil {
			return recoveryResult{}, err
		}
		if scan.maxSeq > res.maxSeq {
			res.maxSeq = scan.maxSeq
		}
		if !scan.torn {
			continue
		}

		// Where the damage is decides what it means. A partial record at the
		// end of the file that was being written is the ordinary shape of a
		// crash, and repairing it silently is correct. The same bytes in a
		// sealed file cannot have that explanation — nothing has written to it
		// since it was fsynced — so it is either media damage or a bug here,
		// and neither is something to paper over.
		//
		// Treating both alike fails either way: as an error, a normal crash
		// leaves a database that will not open; as a truncation, real damage
		// silently discards every record after it.
		if i != len(ids)-1 {
			return recoveryResult{}, fmt.Errorf("%w: %s at offset %d: %w",
				ErrCorrupted, datafileName(id), scan.lastGood, scan.reason)
		}
		res.torn, res.tornAt = true, scan.lastGood
	}
	return res, nil
}

// scanState is the outcome of replaying one data file.
type scanState struct {
	lastGood int64  // offset just past the last intact record
	maxSeq   uint64 // highest seq seen in this file
	torn     bool   // the scan stopped on something that was not a record
	reason   error  // why, when torn
}

// scanDatafile replays one data file into kd.
//
// A truncated or mis-checksummed record is not reported as an error: it is how
// a crash looks, and only the caller knows whether this file is allowed to end
// that way. What comes back is the offset where the intact log stops.
//
// The whole file is read into memory. The scan has to touch every value byte
// whether it keeps it or not, because the checksum covers the value and there
// is no way to verify a record without reading all of it. That cost — a full
// pass over all data at every startup — is the reason hint files exist.
func scanDatafile(path string, id uint32, kd *keydir, opts *options) (scanState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return scanState{}, err
	}

	var st scanState
	for st.lastGood < int64(len(data)) {
		rec, n, err := decodeRecord(data[st.lastGood:], opts.maxKeySize, opts.maxValueSize)
		if err != nil {
			st.torn, st.reason = true, err
			return st, nil
		}
		if rec.seq > st.maxSeq {
			st.maxSeq = rec.seq
		}

		// One comparison resolves overwrites, deletions and duplicate copies
		// of the same record alike: keep whichever version has the higher seq.
		// Sequence numbers are never reused, so two records sharing one are by
		// definition the same record written twice, and ignoring the second is
		// always right.
		if e, ok := kd.get(rec.key); !ok || e.seq < rec.seq {
			if rec.isTombstone() {
				// A deleted key is absent from the index, not marked in it.
				// The tombstone stays on disk, which is the whole point: the
				// index is rebuilt from the log, so a deletion that left no
				// record behind would undo itself on the next restart.
				kd.delete(rec.key)
			} else {
				kd.put(rec.key, keydirEntry{
					fileID: id,
					vsz:    uint32(len(rec.value)),
					vpos:   st.lastGood + headerSize + int64(len(rec.key)),
					seq:    rec.seq,
				})
			}
		}
		st.lastGood += int64(n)
	}
	return st, nil
}
