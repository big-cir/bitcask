package bitcask

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// Reference model comparison.
//
// Every other test in this package states a result that somebody worked out by
// hand, which means it can only check the cases somebody thought of. This one
// states no results at all. It runs a generated sequence of operations against
// the database and against a map[string][]byte, and requires the two to hold
// the same keys and the same bytes after every single operation.
//
// The map is what decides who is wrong. It is one line of the standard library
// per operation, it has no file format, and it has no failure modes.
//
// opCloseOpen is the reason this file exists. A map has no restart, so it
// keeps the right answer across one for free, while the database has to reach
// that same answer by closing its files, replaying the log and rebuilding the
// index out of hint files. A disagreement that appears after opCloseOpen is a
// bug in recovery and nowhere else.
//
// What this does not cover: damaged bytes on disk. Nothing here writes to the
// files behind the database's back — that is what damage_test.go does.

type opKind int

const (
	opPut opKind = iota
	opGet
	opDelete
	opCloseOpen
)

// op is one generated operation. It is also the unit a failure is reported in:
// printing the history gives the exact sequence that produced the mismatch.
//
// key is a string rather than a []byte because the reference map is keyed by
// string, because a failure message prints it directly, and because == works
// on it. It is converted at the two call sites that hand it to the database.
type op struct {
	kind  opKind
	key   string
	value []byte
}

func (o op) String() string {
	switch o.kind {
	case opPut:
		return fmt.Sprintf("Put(%q, %q)", o.key, o.value)
	case opGet:
		return fmt.Sprintf("Get(%q)", o.key)
	case opDelete:
		return fmt.Sprintf("Delete(%q)", o.key)
	default:
		return "Close + Open"
	}
}

// modelKeys is the set of keys the generated operations draw from.
//
// Sixteen is deliberate. With a thousand distinct keys an operation almost
// never lands on a key that is already there, so overwrites, deletions and
// tombstones that shadow an older record would hardly ever be generated at
// all. Sixteen keys over a few hundred operations means every key is written,
// overwritten and deleted many times over.
var modelKeys = buildModelKeys()

func buildModelKeys() []string {
	keys := make([]string, 16)
	for i := range keys {
		keys[i] = fmt.Sprintf("key_%02d", i)
	}
	return keys
}

// decodeOp turns three arbitrary bytes into one operation. Both the seeded
// test and the fuzz target build their operations through this function, so
// the two search the same space and a sequence found by one can be replayed by
// the other.
//
// The mix is 40% Put, 30% Get, 20% Delete, 10% Close+Open. Writes have to
// dominate or there is nothing on disk to read back, and Close+Open has to
// stay rare because it reopens and re-reads the whole database.
func decodeOp(kindByte, keyByte, valueByte byte) op {
	key := modelKeys[int(keyByte)%len(modelKeys)]
	switch n := kindByte % 10; {
	case n < 4:
		return op{kind: opPut, key: key, value: modelValue(key, valueByte)}
	case n < 7:
		return op{kind: opGet, key: key}
	case n < 9:
		return op{kind: opDelete, key: key}
	default:
		return op{kind: opCloseOpen}
	}
}

// modelValue builds the bytes a Put stores.
//
// The value carries its own key inside it. A lookup that reads the right
// number of bytes from the wrong offset then produces a mismatch here, instead
// of returning a neighbouring value of the same length and passing.
//
// One value in twenty-four is empty, which is data the database has to store
// and hand back, and is not the same thing as a key that was deleted.
func modelValue(key string, b byte) []byte {
	if b%24 == 0 {
		return []byte{}
	}
	return []byte(fmt.Sprintf("%s:%d:%s", key, b, strings.Repeat("x", int(b)%24)))
}

// randomScript produces the byte string decodeOp reads, so that a seed and a
// fuzz input are the same thing in two encodings: the seeded test builds its
// script here, the fuzzer is handed one and mutates it.
func randomScript(seed int64, ops int) []byte {
	rng := rand.New(rand.NewSource(seed))
	script := make([]byte, 3*ops)
	for i := range script {
		script[i] = byte(rng.Intn(256))
	}
	return script
}

// modelRun holds one comparison: the database, the map it is checked against,
// and what has been done to both so far.
type modelRun struct {
	t       *testing.T
	dir     string
	opts    []Option
	db      *DB
	model   map[string][]byte
	history []op
	replay  string // what the reader should do to see this failure again
	reopens int
}

func newModelRun(t *testing.T, replay string) *modelRun {
	t.Helper()
	dir := t.TempDir()

	// 512 bytes is small enough that a dozen or so records fill a file.
	// Rotation is what seals a file, sealing is what writes its hint file, and
	// hint files are the path opCloseOpen is here to exercise. At the default
	// 64 MiB nothing would ever rotate and every reopen would replay a single
	// active file.
	opts := []Option{WithMaxFileSize(512)}

	r := &modelRun{
		t:      t,
		dir:    dir,
		opts:   opts,
		db:     mustOpen(t, dir, opts...),
		model:  map[string][]byte{},
		replay: replay,
	}
	t.Cleanup(func() { r.db.Close() })
	return r
}

// run applies every operation in script and compares the two after each one.
func (r *modelRun) run(script []byte) {
	for i := 0; i+2 < len(script); i += 3 {
		r.apply(decodeOp(script[i], script[i+1], script[i+2]))
	}
}

func (r *modelRun) apply(o op) {
	r.history = append(r.history, o)

	switch o.kind {
	case opPut:
		if err := r.db.Put([]byte(o.key), o.value); err != nil {
			r.fatalf("Put(%q) returned %v, want nil", o.key, err)
		}
		r.model[o.key] = o.value

	case opGet:
		r.requireKeyAgrees(o.key)

	case opDelete:
		err := r.db.Delete([]byte(o.key))
		if _, live := r.model[o.key]; live {
			if err != nil {
				r.fatalf("Delete(%q) returned %v, want nil", o.key, err)
			}
			delete(r.model, o.key)
			break
		}
		// Deleting a key that is not there writes no tombstone and reports
		// ErrKeyNotFound. That is a decision this package made rather than
		// something the record format forces, so the generated sequence has to
		// check it.
		if !errors.Is(err, ErrKeyNotFound) {
			r.fatalf("Delete(%q) on an absent key returned %v, want ErrKeyNotFound", o.key, err)
		}

	case opCloseOpen:
		r.reopen()
	}

	r.requireAgreement()
}

func (r *modelRun) reopen() {
	if err := r.db.Close(); err != nil {
		r.fatalf("Close returned %v, want nil", err)
	}
	db, err := Open(r.dir, r.opts...)
	if err != nil {
		r.fatalf("Open returned %v, want nil", err)
	}
	r.db = db
	r.reopens++
}

// requireAgreement compares the whole database against the whole map.
//
// Reading every key of the key space catches both directions of disagreement:
// a live key that reads back wrong or missing, and a deleted key that came
// back from the dead. Len is checked as well, which is what would catch a key
// the database holds that the map has never heard of.
func (r *modelRun) requireAgreement() {
	for _, key := range modelKeys {
		r.requireKeyAgrees(key)
	}
	if got, want := r.db.Len(), len(r.model); got != want {
		r.fatalf("Len() = %d, want %d", got, want)
	}
}

func (r *modelRun) requireKeyAgrees(key string) {
	got, err := r.db.Get([]byte(key))
	want, live := r.model[key]
	switch {
	case !live:
		if !errors.Is(err, ErrKeyNotFound) {
			r.fatalf("Get(%q) = %q, %v; the map holds no such key, want ErrKeyNotFound", key, got, err)
		}
	case err != nil:
		r.fatalf("Get(%q) returned %v; the map holds %q", key, err, want)
	case !bytes.Equal(got, want):
		r.fatalf("Get(%q) = %q; the map holds %q", key, got, want)
	}
}

// requireReopenedThroughHints fails the run unless it did the thing it claims
// to do. A sequence that never reopened the database never entered recovery at
// all, and one that never rotated has no sealed file and therefore no hint
// file, so recovery would have read the data files instead.
func (r *modelRun) requireReopenedThroughHints() {
	r.t.Helper()
	if r.reopens == 0 {
		r.t.Fatalf("the sequence never reopened the database, so recovery was never run")
	}
	requireHintsUsable(r.t, r.dir)
}

// fatalf reports the mismatch with everything needed to see it again: the
// command that replays this sequence, and the tail of the sequence itself.
func (r *modelRun) fatalf(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("%s\n\nafter %d operation(s) and %d reopen(s)\nreplay with: %s\n%s",
		fmt.Sprintf(format, args...), len(r.history), r.reopens, r.replay, r.historyTail(25))
}

func (r *modelRun) historyTail(n int) string {
	from := len(r.history) - n
	if from < 0 {
		from = 0
	}
	var b strings.Builder
	if from > 0 {
		fmt.Fprintf(&b, "last %d of %d operations:\n", n, len(r.history))
	} else {
		fmt.Fprintf(&b, "%d operations:\n", len(r.history))
	}
	for i, o := range r.history[from:] {
		fmt.Fprintf(&b, "  %4d  %s\n", from+i, o)
	}
	return b.String()
}

// TestModel runs fixed sequences. These are the ones that run on every
// go test; FuzzModel below is where new sequences come from.
//
// The seeds are arbitrary numbers, but fixing them is not arbitrary: a failure
// has to come back when the test is rerun, without depending on a saved input
// file being present.
//
// Most of the wall clock here is fsync: every rotation and every Close forces
// the active file to disk, and a sequence of 400 operations does that about
// fifty times. That is why -short cuts the sequences down rather than skipping
// the test — a shorter sequence still reopens the database a dozen times.
func TestModel(t *testing.T) {
	seeds := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	opsPerSeed := 400
	if testing.Short() {
		seeds, opsPerSeed = seeds[:3], 200
	}

	for _, seed := range seeds {
		name := fmt.Sprintf("seed_%d", seed)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := newModelRun(t, fmt.Sprintf("go test -run 'TestModel/%s' .", name))
			run.run(randomScript(seed, opsPerSeed))
			run.requireReopenedThroughHints()
		})
	}
}

// maxFuzzOps caps how long a generated sequence may get. The fuzzer hands over
// inputs of any length, and one operation in ten closes and reopens the
// database, which forces a file to disk and then reads every file back. At 100
// operations one execution takes about 70 ms on an APFS laptop; letting the
// input grow without a limit turns that into whole seconds per execution and
// the search stops moving.
const maxFuzzOps = 100

// FuzzModel is the same comparison, driven by the fuzzer instead of a seed.
//
// What the fuzzer adds is memory between runs. It records which lines of this
// package each input reached, keeps the inputs that reached a line nothing had
// reached before, and produces the next inputs by changing a few bytes of
// those. So a sequence that first managed to get a tombstone into a hint file
// is kept and varied. A seed cannot be varied that way: seed+1 produces an
// unrelated sequence, not a similar one.
//
// New sequences are only searched for when the fuzzer is asked to:
//
//	go test -fuzz=FuzzModel -fuzztime=60s -fuzzminimizetime=5s .
//
// -fuzzminimizetime is not optional here, and leaving it out is not a smaller
// version of the same run. Every time the fuzzer finds an input that reached a
// new line, it shortens that input by rerunning it hundreds of times, and its
// default budget for doing so is 60 seconds per input. One execution of this
// target costs about 70 ms, where a parser target costs microseconds, so the
// first shortening alone outlasts a one-minute run: without the flag,
// -fuzztime=60s ends in "context deadline exceeded" after 30 executions. With
// it set to 5 seconds the same minute gets through about 700.
//
// A plain go test replays the corpus below plus any failing input saved under
// testdata/fuzz/FuzzModel/, and searches for nothing.
//
// This target does not call requireReopenedThroughHints. An input of six bytes
// is two operations and rotates nothing, and failing it would only mean the
// fuzzer had generated a short input. TestModel is what pins down that a
// sequence reaches the hint files.
func FuzzModel(f *testing.F) {
	for _, seed := range []int64{1, 2, 3} {
		f.Add(randomScript(seed, 60))
	}

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 3*maxFuzzOps {
			script = script[:3*maxFuzzOps]
		}
		run := newModelRun(t, "the input is saved under testdata/fuzz/FuzzModel/, and go test replays it")
		run.run(script)
	})
}
