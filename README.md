# Bitcask

An embedded key-value storage engine based on the Bitcask design: append-only
data files plus an in-memory index that maps every key to the location of its
value on disk.

The purpose of this repository is to understand why that design needs
append-only logs, an in-memory key directory, checksums, crash recovery, and
compaction — and to arrive at each of those by building it.

## Storage model

Records are appended to the end of the active data file. Nothing is ever
modified in place.

- `Put` appends a new record and points the index at it.
- The in-memory key directory maps each key to the file, offset, and length of
  its latest value.
- `Get` looks the location up in memory and reads only the value bytes from
  disk, so a lookup costs one hash lookup and one read.
- `Delete` appends a tombstone record. The superseded bytes stay on disk.
- Opening the database replays the data files to rebuild the key directory.
  Nothing about the index is stored.

The trade-offs are deliberate:

- Every key must fit in memory. Total value size is bounded only by disk.
- Overwritten and deleted values stay on disk until compaction reclaims them.
- Ordered iteration and range scans are not possible, because the index is a
  hash.
- One writer at a time. Readers are not limited.

## Record format

The format is fixed, so a database written by one implementation can be read by
another. Integers are little-endian.

```
 0        4              12        16        20     21
 ├────────┼──────────────┼─────────┼─────────┼──────┼──────────┬────────────┐
 │ crc32c │     seq      │   ksz   │   vsz   │flags │   key    │   value    │
 │  (4)   │     (8)      │   (4)   │   (4)   │ (1)  │  (ksz)   │   (vsz)    │
 └────────┴──────────────┴─────────┴─────────┴──────┴──────────┴────────────┘
 └──────────── fixed header, 21 bytes ─────────────┘
```

| field | meaning |
|---|---|
| `crc32c` | CRC-32/Castagnoli over everything after it, `seq` through the end of the value |
| `seq` | monotonic sequence number; the record with the highest `seq` for a key is its current value |
| `ksz` / `vsz` | key and value lengths. A record occupies `21 + ksz + vsz` bytes |
| `flags` | bit 0 marks a tombstone. A tombstone has `vsz == 0` and no value bytes |

There are no separators in a data file. A record's extent is computed from the
lengths in its own header, which is what makes a front-to-back scan possible.

**Ordering uses `seq`, not a wall-clock timestamp.** This departs from the
original paper. Wall clocks move backwards (clock adjustment, restoring a
virtual machine snapshot), two writes in the same tick tie, and compaction
rewrites old records into new files, so file order cannot break the tie either.
With `seq`, one rule — highest `seq` for a key wins — resolves overwrites,
deletions, and duplicate copies of a record alike.

## Durability model

A process crash and a power loss are different failures and are handled
differently.

**Process crash.** Data handed to the operating system with a write call sits
in the page cache, which outlives the process. Opening the database replays the
data files and rebuilds the index. If the file that was being written ends with
a partial record, recovery truncates it back to the last intact record: those
bytes never returned success to a caller.

Damage inside a sealed file has no crash to explain it, since nothing has
written to that file since it was synced. Opening reports an error instead of
discarding everything after the damaged record.

**Power loss.** Data that has not been synced to physical storage may be lost.
Syncing on every write costs two to three orders of magnitude in throughput —
measured at 724x on macOS, where a sync also flushes the drive's own cache — so
when it happens is a policy decision, and the guarantee changes with it.

The checksum is what makes the first case work at all. Without it there is no
way to tell a record written by the engine from leftover bytes whose length
fields happen to read as plausible numbers, and therefore no way to decide
where the intact log ends.

## Scope

This is a learning project. The code is kept small on purpose so that the
on-disk format, the recovery path, and the boundary between "written" and
"durable" can be read directly rather than inferred.
