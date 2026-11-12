package log

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every test record uses this key and value, so each encoded record is exactly
// 33 + 4 + 8 = 45 bytes. With MaxSegSize = 90, a segment is full after 2 records.
var (
	testKey   = []byte("0712")
	testValue = []byte("Paid 500")
)

const (
	recordSize    = 45
	bigSegment    = 1 << 20        // large enough that nothing rolls
	twoPerSegment = 2 * recordSize // full after exactly 2 records
)

func segmentName(base uint64, ext string) string {
	return fmt.Sprintf("%020d%s", base, ext)
}

// openLog opens a log and closes it when the test ends. Closing twice is
// harmless here, so tests that close and reopen don't need to worry about it.
func openLog(t *testing.T, dir string, maxSegSize int64) *Log {
	t.Helper()
	l, err := Open(dir, Config{MaxSegSize: maxSegSize})
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// appendN appends n records and checks the offsets come back as start, start+1, ...
func appendN(t *testing.T, l *Log, n int, start uint64) {
	t.Helper()
	for i := 0; i < n; i++ {
		want := start + uint64(i)
		got, err := l.Append(testKey, testValue)
		if err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
		if got != want {
			t.Fatalf("Append #%d returned offset %d, want %d", i, got, want)
		}
	}
}

// segmentBases returns the base offsets of the .log files on disk, sorted.
func segmentBases(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") && len(e.Name()) == 24 {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func readAll(t *testing.T, l *Log, from, to uint64) {
	t.Helper()
	for off := from; off <= to; off++ {
		rec, err := l.Read(off)
		if err != nil {
			t.Fatalf("Read(%d): %v", off, err)
		}
		if rec.Offset != off {
			t.Fatalf("Read(%d) returned record with offset %d", off, rec.Offset)
		}
		if !bytes.Equal(rec.Key, testKey) || !bytes.Equal(rec.Value, testValue) {
			t.Fatalf("Read(%d) = key %q value %q", off, rec.Key, rec.Value)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. Opening
// ---------------------------------------------------------------------------

func TestOpenCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "payments-0") // does not exist yet
	l := openLog(t, dir, bigSegment)

	for _, ext := range []string{".log", ".index"} {
		if _, err := os.Stat(filepath.Join(dir, segmentName(0, ext))); err != nil {
			t.Errorf("first segment file %s missing: %v", ext, err)
		}
	}
	if got := l.LowestOffset(); got != 0 {
		t.Errorf("LowestOffset() = %d, want 0", got)
	}
	if _, err := l.HighestOffset(); !errors.Is(err, ErrLogEmpty) {
		t.Errorf("HighestOffset() on empty log: err = %v, want ErrLogEmpty", err)
	}
}

func TestOpenIgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"notes.txt", "notes.log", "readme"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("junk"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir.log"), 0755); err != nil {
		t.Fatal(err)
	}

	l := openLog(t, dir, bigSegment)
	appendN(t, l, 3, 0)
	readAll(t, l, 0, 2)
}

func TestOpenRejectsZeroMaxSegSize(t *testing.T) {
	l, err := Open(t.TempDir(), Config{MaxSegSize: 0})
	if err == nil {
		_ = l.Close()
		t.Fatal("Open with MaxSegSize 0 succeeded, want an error")
	}
}

// ---------------------------------------------------------------------------
// 2. Appending and reading
// ---------------------------------------------------------------------------

func TestAppendReturnsSequentialOffsets(t *testing.T) {
	l := openLog(t, t.TempDir(), bigSegment)
	appendN(t, l, 5, 0)

	high, err := l.HighestOffset()
	if err != nil || high != 4 {
		t.Fatalf("HighestOffset() = %d, %v; want 4, nil", high, err)
	}
}

func TestAppendThenReadReturnsSameRecord(t *testing.T) {
	l := openLog(t, t.TempDir(), bigSegment)

	before := time.Now().UnixMilli()
	off, err := l.Append([]byte("0745"), []byte("Paid KES 1,000"))
	after := time.Now().UnixMilli()
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	rec, err := l.Read(off)
	if err != nil {
		t.Fatalf("Read(%d): %v", off, err)
	}
	if string(rec.Key) != "0745" || string(rec.Value) != "Paid KES 1,000" {
		t.Errorf("Read = key %q value %q", rec.Key, rec.Value)
	}
	if rec.Timestamp < before || rec.Timestamp > after {
		t.Errorf("Timestamp = %d, want between %d and %d (Unix milliseconds)", rec.Timestamp, before, after)
	}
}

func TestAppendBinaryAndEmptyValues(t *testing.T) {
	l := openLog(t, t.TempDir(), bigSegment)
	cases := []struct{ key, value []byte }{
		{nil, []byte("no key")},
		{[]byte("k"), nil},
		{[]byte{0, 1, 2}, []byte{0, 0, 255, 0}},
	}
	for i, c := range cases {
		off, err := l.Append(c.key, c.value)
		if err != nil {
			t.Fatalf("Append case %d: %v", i, err)
		}
		rec, err := l.Read(off)
		if err != nil {
			t.Fatalf("Read case %d: %v", i, err)
		}
		if !bytes.Equal(rec.Key, c.key) || !bytes.Equal(rec.Value, c.value) {
			t.Errorf("case %d: got key %v value %v, want key %v value %v", i, rec.Key, rec.Value, c.key, c.value)
		}
	}
}

func TestReadOutOfRange(t *testing.T) {
	l := openLog(t, t.TempDir(), bigSegment)

	if _, err := l.Read(0); !errors.Is(err, ErrOffsetOutOfRange) {
		t.Errorf("Read(0) on empty log: err = %v, want ErrOffsetOutOfRange", err)
	}

	appendN(t, l, 3, 0)
	for _, off := range []uint64{3, 4, 1000} {
		if _, err := l.Read(off); !errors.Is(err, ErrOffsetOutOfRange) {
			t.Errorf("Read(%d): err = %v, want ErrOffsetOutOfRange", off, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Rolling to new segments
// ---------------------------------------------------------------------------

func TestAppendRollsIntoNewSegments(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir, twoPerSegment)
	appendN(t, l, 5, 0)

	want := []string{segmentName(0, ".log"), segmentName(2, ".log"), segmentName(4, ".log")}
	got := segmentBases(t, dir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("segments on disk = %v, want %v (no gaps, no overlaps)", got, want)
	}
	readAll(t, l, 0, 4)

	high, err := l.HighestOffset()
	if err != nil || high != 4 {
		t.Fatalf("HighestOffset() = %d, %v; want 4, nil (not the active segment's base offset)", high, err)
	}
}

func TestReopenContinuesWhereItStopped(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, Config{MaxSegSize: twoPerSegment})
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 5, 0)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	l = openLog(t, dir, twoPerSegment)
	if got := l.LowestOffset(); got != 0 {
		t.Errorf("LowestOffset() after reopen = %d, want 0", got)
	}
	if high, err := l.HighestOffset(); err != nil || high != 4 {
		t.Fatalf("HighestOffset() after reopen = %d, %v; want 4, nil", high, err)
	}
	readAll(t, l, 0, 4)

	appendN(t, l, 2, 5) // continues at 5, 6
	readAll(t, l, 0, 6)
}

// ---------------------------------------------------------------------------
// 4. Crash recovery
// ---------------------------------------------------------------------------

// writeThenClose appends n records to a fresh log in dir and closes it.
func writeThenClose(t *testing.T, dir string, n int) {
	t.Helper()
	l, err := Open(dir, Config{MaxSegSize: bigSegment})
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, n, 0)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRecoveryRemovesTornLastRecord(t *testing.T) {
	dir := t.TempDir()
	writeThenClose(t, dir, 3)

	// Simulate a crash mid-write: the last record lost its final 5 bytes.
	logPath := filepath.Join(dir, segmentName(0, ".log"))
	if err := os.Truncate(logPath, 3*recordSize-5); err != nil {
		t.Fatal(err)
	}

	l := openLog(t, dir, bigSegment)
	if high, err := l.HighestOffset(); err != nil || high != 1 {
		t.Fatalf("HighestOffset() = %d, %v; want 1, nil", high, err)
	}
	readAll(t, l, 0, 1)
	if _, err := l.Read(2); !errors.Is(err, ErrOffsetOutOfRange) {
		t.Errorf("Read(2) after recovery: err = %v, want ErrOffsetOutOfRange", err)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2*recordSize {
		t.Errorf(".log size after recovery = %d, want %d (torn bytes truncated)", info.Size(), 2*recordSize)
	}

	// The next append reuses offset 2 cleanly.
	appendN(t, l, 1, 2)
	readAll(t, l, 0, 2)
}

func TestRecoveryRestoresMissingIndexEntry(t *testing.T) {
	dir := t.TempDir()
	writeThenClose(t, dir, 3)

	// Simulate a crash between writing the record and its index entry.
	indexPath := filepath.Join(dir, segmentName(0, ".index"))
	if err := os.Truncate(indexPath, 2*8); err != nil {
		t.Fatal(err)
	}

	l := openLog(t, dir, bigSegment)
	if high, err := l.HighestOffset(); err != nil || high != 2 {
		t.Fatalf("HighestOffset() = %d, %v; want 2, nil (index rebuilt from .log)", high, err)
	}
	readAll(t, l, 0, 2)
	appendN(t, l, 1, 3)
}

func TestRecoveryDropsCorruptLastRecord(t *testing.T) {
	dir := t.TempDir()
	writeThenClose(t, dir, 3)

	// Flip a byte inside the last record's value, so its checksum fails.
	logPath := filepath.Join(dir, segmentName(0, ".log"))
	f, err := os.OpenFile(logPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	pos := int64(3*recordSize - 2)
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, pos); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, pos); err != nil {
		t.Fatal(err)
	}
	f.Close()

	l := openLog(t, dir, bigSegment)
	if high, err := l.HighestOffset(); err != nil || high != 1 {
		t.Fatalf("HighestOffset() = %d, %v; want 1, nil (corrupt record dropped)", high, err)
	}
	readAll(t, l, 0, 1)
}

func TestRecoveryLeavesHealthyLogUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeThenClose(t, dir, 3)

	l := openLog(t, dir, bigSegment)
	if high, err := l.HighestOffset(); err != nil || high != 2 {
		t.Fatalf("HighestOffset() = %d, %v; want 2, nil", high, err)
	}
	readAll(t, l, 0, 2)
}

// ---------------------------------------------------------------------------
// 5. Closing
// ---------------------------------------------------------------------------

func TestReadAfterCloseFails(t *testing.T) {
	l, err := Open(t.TempDir(), Config{MaxSegSize: twoPerSegment})
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 5, 0)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, off := range []uint64{0, 2, 4} { // one offset in every segment
		if _, err := l.Read(off); err == nil {
			t.Errorf("Read(%d) after Close succeeded; every segment should be closed", off)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. Concurrency (run with: go test -race)
// ---------------------------------------------------------------------------

func TestConcurrentAppendsAndReads(t *testing.T) {
	l := openLog(t, t.TempDir(), twoPerSegment*5) // rolls every 10 records

	const writers, perWriter = 8, 50
	offsets := make(chan uint64, writers*perWriter)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				off, err := l.Append(testKey, testValue)
				if err != nil {
					t.Errorf("Append: %v", err)
					return
				}
				offsets <- off
			}
		}()
	}

	// Readers run alongside the writers, touching every read-only method.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = l.LowestOffset()
				if high, err := l.HighestOffset(); err == nil {
					if _, err := l.Read(high); err != nil {
						t.Errorf("Read(%d) during appends: %v", high, err)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	readers.Wait()
	close(offsets)

	// Every offset 0..399 must have been handed out exactly once.
	seen := make(map[uint64]bool)
	for off := range offsets {
		if seen[off] {
			t.Fatalf("offset %d was returned twice", off)
		}
		seen[off] = true
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("got %d distinct offsets, want %d", len(seen), writers*perWriter)
	}
	readAll(t, l, 0, writers*perWriter-1)
}
