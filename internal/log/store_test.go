package log

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The store only cares about the 4-byte length prefix, so these tests build
// raw length-prefixed byte slices instead of full encoded records.
func framed(payload string) []byte {
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	copy(buf[4:], payload)
	return buf
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), segmentName(0, ".log"))
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// Three records of 4+5, 4+11 and 4+3 bytes: positions 0, 9 and 24, total 31.
var storeSamples = [][]byte{framed("hello"), framed("Paid KES 50"), framed("bye")}

func appendStoreSamples(t *testing.T, s *Store) []int64 {
	t.Helper()
	var positions []int64
	for i, rec := range storeSamples {
		pos, err := s.Append(rec)
		if err != nil {
			t.Fatalf("Append sample %d: %v", i, err)
		}
		positions = append(positions, pos)
	}
	return positions
}

func TestStoreNewCreatesEmptyFile(t *testing.T) {
	s, path := newTestStore(t)

	if got := s.Size(); got != 0 {
		t.Fatalf("Size() = %d, want 0", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("store file not created: %v", err)
	}
}

func TestStoreAppendReturnsPositions(t *testing.T) {
	s, path := newTestStore(t)
	positions := appendStoreSamples(t, s)

	want := []int64{0, 9, 24}
	for i := range want {
		if positions[i] != want[i] {
			t.Errorf("Append #%d returned position %d, want %d", i, positions[i], want[i])
		}
	}
	if got := s.Size(); got != 31 {
		t.Errorf("Size() = %d, want 31", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 31 {
		t.Errorf("file size on disk = %d, want 31", info.Size())
	}
}

func TestStoreReadReturnsWholeRecord(t *testing.T) {
	s, _ := newTestStore(t)
	positions := appendStoreSamples(t, s)

	for i, pos := range positions {
		got, err := s.Read(pos)
		if err != nil {
			t.Fatalf("Read(%d): %v", pos, err)
		}
		if !bytes.Equal(got, storeSamples[i]) {
			t.Errorf("Read(%d) = %v, want %v (including the 4-byte length prefix)", pos, got, storeSamples[i])
		}
	}
}

func TestStoreReadOutOfRange(t *testing.T) {
	s, _ := newTestStore(t)

	if _, err := s.Read(0); !errors.Is(err, ErrStorePositionOutOfRange) {
		t.Errorf("Read(0) on empty store: err = %v, want ErrStorePositionOutOfRange", err)
	}

	appendStoreSamples(t, s) // size is now 31
	for _, pos := range []int64{-1, 31, 29, 1000} {
		if _, err := s.Read(pos); !errors.Is(err, ErrStorePositionOutOfRange) {
			t.Errorf("Read(%d): err = %v, want ErrStorePositionOutOfRange", pos, err)
		}
	}
}

func TestStoreReadDetectsIncompleteRecord(t *testing.T) {
	s, _ := newTestStore(t)
	appendStoreSamples(t, s)

	// A torn write: the length says 41 bytes follow, but only 10 made it.
	torn := make([]byte, 4+10)
	binary.BigEndian.PutUint32(torn[0:4], 41)
	pos, err := s.Append(torn)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Read(pos); !errors.Is(err, ErrStoreIncompleteRecord) {
		t.Fatalf("Read of torn record: err = %v, want ErrStoreIncompleteRecord", err)
	}
	// Earlier records are unaffected.
	if got, err := s.Read(9); err != nil || !bytes.Equal(got, storeSamples[1]) {
		t.Fatalf("Read(9) = %v, %v; want the second sample", got, err)
	}
}

func TestStoreReadRejectsHugeLength(t *testing.T) {
	s, _ := newTestStore(t)

	// Corrupt length field claiming about 4 GB; must be rejected without
	// trying to allocate that much memory.
	bad := []byte{0xFF, 0xFF, 0xFF, 0xFF, 1, 2, 3}
	pos, err := s.Append(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(pos); !errors.Is(err, ErrStoreRecordTooLarge) {
		t.Fatalf("Read with huge length: err = %v, want ErrStoreRecordTooLarge", err)
	}
}

func TestStoreReopenKeepsData(t *testing.T) {
	s, path := newTestStore(t)
	positions := appendStoreSamples(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reopen): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if got := reopened.Size(); got != 31 {
		t.Fatalf("Size() after reopen = %d, want 31", got)
	}
	for i, pos := range positions {
		got, err := reopened.Read(pos)
		if err != nil || !bytes.Equal(got, storeSamples[i]) {
			t.Fatalf("Read(%d) after reopen = %v, %v", pos, got, err)
		}
	}

	// New appends go to the end, not over existing data.
	pos, err := reopened.Append(framed("next"))
	if err != nil || pos != 31 {
		t.Fatalf("Append after reopen = %d, %v; want 31, nil", pos, err)
	}
}

func TestStoreTruncate(t *testing.T) {
	s, path := newTestStore(t)
	appendStoreSamples(t, s)

	// Cut back to the end of the first record.
	if err := s.Truncate(9); err != nil {
		t.Fatalf("Truncate(9): %v", err)
	}
	if got := s.Size(); got != 9 {
		t.Errorf("Size() after Truncate = %d, want 9", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 9 {
		t.Errorf("file size on disk after Truncate = %d, want 9", info.Size())
	}
	if _, err := s.Read(9); !errors.Is(err, ErrStorePositionOutOfRange) {
		t.Errorf("Read(9) after Truncate: err = %v, want ErrStorePositionOutOfRange", err)
	}
	if got, err := s.Read(0); err != nil || !bytes.Equal(got, storeSamples[0]) {
		t.Errorf("Read(0) after Truncate = %v, %v; want first sample", got, err)
	}

	// The next append lands right after the remaining data.
	if pos, err := s.Append(framed("again")); err != nil || pos != 9 {
		t.Fatalf("Append after Truncate = %d, %v; want 9, nil", pos, err)
	}

	// Truncating to a negative size or past the end is rejected.
	for _, size := range []int64{-1, s.Size() + 1} {
		if err := s.Truncate(size); err == nil {
			t.Errorf("Truncate(%d) succeeded, want an error", size)
		}
	}
}

func TestStoreSync(t *testing.T) {
	s, _ := newTestStore(t)
	appendStoreSamples(t, s)
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func TestStoreConcurrentReads(t *testing.T) {
	s, _ := newTestStore(t)
	positions := appendStoreSamples(t, s)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				for i, pos := range positions {
					got, err := s.Read(pos)
					if err != nil || !bytes.Equal(got, storeSamples[i]) {
						t.Errorf("concurrent Read(%d) = %v, %v", pos, got, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
