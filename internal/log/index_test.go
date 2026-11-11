package log

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newTestIndex creates an index in a fresh temp directory and closes it when the test ends.
func newTestIndex(t *testing.T) (*Index, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "00000000000000000000.index")
	idx, err := NewIndex(path)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx, path
}

// sampleEntries are relative offset -> position pairs for records of 45, 47 and 52 bytes.
var sampleEntries = []struct {
	relOffset uint32
	pos       uint32
}{
	{0, 0},
	{1, 45},
	{2, 92},
}

func writeSamples(t *testing.T, idx *Index) {
	t.Helper()
	for _, e := range sampleEntries {
		if err := idx.Write(e.relOffset, e.pos); err != nil {
			t.Fatalf("Write(%d, %d): %v", e.relOffset, e.pos, err)
		}
	}
}

func TestNewIndexCreatesEmptyIndex(t *testing.T) {
	idx, path := newTestIndex(t)

	if got := idx.Entries(); got != 0 {
		t.Fatalf("Entries() = %d, want 0", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("index file was not created: %v", err)
	}
}

func TestWriteThenRead(t *testing.T) {
	idx, _ := newTestIndex(t)
	writeSamples(t, idx)

	if got := idx.Entries(); got != uint32(len(sampleEntries)) {
		t.Fatalf("Entries() = %d, want %d", got, len(sampleEntries))
	}
	for _, e := range sampleEntries {
		pos, err := idx.Read(e.relOffset)
		if err != nil {
			t.Fatalf("Read(%d): %v", e.relOffset, err)
		}
		if pos != e.pos {
			t.Errorf("Read(%d) = %d, want %d", e.relOffset, pos, e.pos)
		}
	}
}

func TestFileSizeMatchesEntries(t *testing.T) {
	idx, path := newTestIndex(t)
	writeSamples(t, idx)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	want := int64(len(sampleEntries) * entryWidth)
	if info.Size() != want {
		t.Fatalf("file size = %d, want %d", info.Size(), want)
	}
}

func TestReadOutOfRange(t *testing.T) {
	idx, _ := newTestIndex(t)

	if _, err := idx.Read(0); !errors.Is(err, InvalidEntryError) {
		t.Fatalf("Read(0) on empty index: err = %v, want InvalidEntryError", err)
	}

	writeSamples(t, idx)
	for _, rel := range []uint32{3, 4, 100} {
		if _, err := idx.Read(rel); !errors.Is(err, InvalidEntryError) {
			t.Errorf("Read(%d): err = %v, want InvalidEntryError", rel, err)
		}
	}
}

func TestWriteOutOfOrder(t *testing.T) {
	idx, _ := newTestIndex(t)

	// First entry must be relative offset 0.
	if err := idx.Write(5, 0); !errors.Is(err, InvalidEntryError) {
		t.Fatalf("Write(5, 0) on empty index: err = %v, want InvalidEntryError", err)
	}

	writeSamples(t, idx)

	// Skipping ahead and repeating an existing offset must both be rejected.
	for _, rel := range []uint32{5, 2, 0} {
		if err := idx.Write(rel, 200); !errors.Is(err, InvalidEntryError) {
			t.Errorf("Write(%d, 200): err = %v, want InvalidEntryError", rel, err)
		}
	}

	// A rejected write must not change the entry count.
	if got := idx.Entries(); got != uint32(len(sampleEntries)) {
		t.Fatalf("Entries() after rejected writes = %d, want %d", got, len(sampleEntries))
	}
}

func TestReopenKeepsEntries(t *testing.T) {
	idx, path := newTestIndex(t)
	writeSamples(t, idx)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewIndex(path)
	if err != nil {
		t.Fatalf("NewIndex (reopen): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if got := reopened.Entries(); got != uint32(len(sampleEntries)) {
		t.Fatalf("Entries() after reopen = %d, want %d", got, len(sampleEntries))
	}
	for _, e := range sampleEntries {
		pos, err := reopened.Read(e.relOffset)
		if err != nil {
			t.Fatalf("Read(%d) after reopen: %v", e.relOffset, err)
		}
		if pos != e.pos {
			t.Errorf("Read(%d) after reopen = %d, want %d", e.relOffset, pos, e.pos)
		}
	}

	// Writing continues exactly where it left off.
	if err := reopened.Write(3, 144); err != nil {
		t.Fatalf("Write(3, 144) after reopen: %v", err)
	}
	if pos, err := reopened.Read(3); err != nil || pos != 144 {
		t.Fatalf("Read(3) = %d, %v; want 144, nil", pos, err)
	}
}

func TestPartialEntryIsTruncatedOnOpen(t *testing.T) {
	idx, path := newTestIndex(t)
	writeSamples(t, idx)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a crash mid-write: 5 bytes of a 4th entry reached the disk.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.Write([]byte{0, 0, 0, 3, 0}); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	f.Close()

	reopened, err := NewIndex(path)
	if err != nil {
		t.Fatalf("NewIndex after partial write: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if got := reopened.Entries(); got != uint32(len(sampleEntries)) {
		t.Fatalf("Entries() = %d, want %d", got, len(sampleEntries))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if want := int64(len(sampleEntries) * entryWidth); info.Size() != want {
		t.Fatalf("file size after reopen = %d, want %d (partial entry not truncated)", info.Size(), want)
	}

	// The next write lands cleanly after the last complete entry.
	if err := reopened.Write(3, 144); err != nil {
		t.Fatalf("Write(3, 144): %v", err)
	}
	if pos, err := reopened.Read(3); err != nil || pos != 144 {
		t.Fatalf("Read(3) = %d, %v; want 144, nil", pos, err)
	}
}

func TestCorruptEntryDetected(t *testing.T) {
	idx, path := newTestIndex(t)
	writeSamples(t, idx)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Overwrite the stored relative offset of entry 1 with a wrong value.
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	var bad [4]byte
	binary.BigEndian.PutUint32(bad[:], 99)
	if _, err := f.WriteAt(bad[:], 1*entryWidth); err != nil {
		t.Fatalf("corrupt entry: %v", err)
	}
	f.Close()

	reopened, err := NewIndex(path)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if _, err := reopened.Read(1); !errors.Is(err, ErrIndexCorrupt) {
		t.Fatalf("Read(1) on corrupted entry: err = %v, want ErrIndexCorrupt", err)
	}
	// Untouched entries still read correctly.
	if pos, err := reopened.Read(2); err != nil || pos != 92 {
		t.Fatalf("Read(2) = %d, %v; want 92, nil", pos, err)
	}
}

func TestSync(t *testing.T) {
	idx, _ := newTestIndex(t)
	writeSamples(t, idx)
	if err := idx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}
