package log

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Segment tests use base offset 4, so they also check that the segment
// converts between real offsets (4, 5, 6...) and relative ones (0, 1, 2...).
const segBase = 4

func newTestSegment(t *testing.T, dir string, maxBytes int64) *Segment {
	t.Helper()
	seg, err := NewSegment(dir, maxBytes, segBase)
	if err != nil {
		t.Fatalf("NewSegment: %v", err)
	}
	t.Cleanup(func() { _ = seg.Close() })
	return seg
}

func segRecord(offset uint64) Record {
	return Record{
		Offset:    offset,
		Timestamp: 1_731_230_000_000 + int64(offset),
		Key:       []byte("0712"),
		Value:     []byte(fmt.Sprintf("Paid %03d", offset)), // 8 bytes, so each record is 45 bytes
	}
}

// appendSegRecords appends records with offsets from..to and checks each
// returned offset.
func appendSegRecords(t *testing.T, seg *Segment, from, to uint64) {
	t.Helper()
	for off := from; off <= to; off++ {
		got, err := seg.Append(segRecord(off))
		if err != nil {
			t.Fatalf("Append(offset %d): %v", off, err)
		}
		if got != off {
			t.Fatalf("Append(offset %d) returned %d", off, got)
		}
	}
}

func checkSegRecord(t *testing.T, seg *Segment, off uint64) {
	t.Helper()
	got, err := seg.Read(off)
	if err != nil {
		t.Fatalf("Read(%d): %v", off, err)
	}
	want := segRecord(off)
	if got.Offset != want.Offset || got.Timestamp != want.Timestamp ||
		!bytes.Equal(got.Key, want.Key) || !bytes.Equal(got.Value, want.Value) {
		t.Fatalf("Read(%d) = %+v, want %+v", off, got, want)
	}
}

func segFileSize(t *testing.T, dir, ext string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, segmentName(segBase, ext)))
	if err != nil {
		t.Fatalf("stat %s: %v", ext, err)
	}
	return info.Size()
}

func TestSegmentNewIsEmpty(t *testing.T) {
	dir := t.TempDir()
	seg := newTestSegment(t, dir, bigSegment)

	if got := seg.NextOffset(); got != segBase {
		t.Errorf("NextOffset() = %d, want %d", got, segBase)
	}
	if seg.BaseOffset != segBase {
		t.Errorf("BaseOffset = %d, want %d", seg.BaseOffset, segBase)
	}
	if seg.IsFull() {
		t.Error("new segment reports IsFull() = true")
	}
	// Files are named by the base offset, zero-padded to 20 digits.
	for _, ext := range []string{".log", ".index"} {
		if _, err := os.Stat(filepath.Join(dir, "00000000000000000004"+ext)); err != nil {
			t.Errorf("file 00000000000000000004%s missing: %v", ext, err)
		}
	}
}

func TestSegmentAppendThenRead(t *testing.T) {
	seg := newTestSegment(t, t.TempDir(), bigSegment)
	appendSegRecords(t, seg, 4, 6)

	if got := seg.NextOffset(); got != 7 {
		t.Fatalf("NextOffset() = %d, want 7", got)
	}
	for off := uint64(4); off <= 6; off++ {
		checkSegRecord(t, seg, off)
	}
}

func TestSegmentBinaryAndEmptyValues(t *testing.T) {
	seg := newTestSegment(t, t.TempDir(), bigSegment)
	cases := []Record{
		{Offset: 4, Key: nil, Value: []byte("no key")},
		{Offset: 5, Key: []byte("k"), Value: nil},
		{Offset: 6, Key: []byte{0, 1, 2}, Value: []byte{0, 0, 255, 0}},
	}
	for _, rec := range cases {
		if _, err := seg.Append(rec); err != nil {
			t.Fatalf("Append(offset %d): %v", rec.Offset, err)
		}
	}
	for _, want := range cases {
		got, err := seg.Read(want.Offset)
		if err != nil {
			t.Fatalf("Read(%d): %v", want.Offset, err)
		}
		if !bytes.Equal(got.Key, want.Key) || !bytes.Equal(got.Value, want.Value) {
			t.Errorf("Read(%d) = key %v value %v, want key %v value %v",
				want.Offset, got.Key, got.Value, want.Key, want.Value)
		}
	}
}

func TestSegmentReadOutOfRange(t *testing.T) {
	seg := newTestSegment(t, t.TempDir(), bigSegment)

	if _, err := seg.Read(segBase); !errors.Is(err, ErrSegmentOffsetOutOfRange) {
		t.Errorf("Read(%d) on empty segment: err = %v, want ErrSegmentOffsetOutOfRange", segBase, err)
	}

	appendSegRecords(t, seg, 4, 6)
	// 0 and 3 are below the base offset (a naive subtraction would wrap
	// around); 7 and beyond have not been written.
	for _, off := range []uint64{0, 3, 7, 1000} {
		if _, err := seg.Read(off); !errors.Is(err, ErrSegmentOffsetOutOfRange) {
			t.Errorf("Read(%d): err = %v, want ErrSegmentOffsetOutOfRange", off, err)
		}
	}
}

func TestSegmentAppendOutOfOrder(t *testing.T) {
	dir := t.TempDir()
	seg := newTestSegment(t, dir, bigSegment)

	// The first record must have the base offset.
	if _, err := seg.Append(segRecord(0)); !errors.Is(err, ErrSegmentOffsetOutOfOrder) {
		t.Fatalf("Append(offset 0) to segment 4: err = %v, want ErrSegmentOffsetOutOfOrder", err)
	}

	appendSegRecords(t, seg, 4, 6)
	logSize := segFileSize(t, dir, ".log")

	// Skipping ahead, repeating and going backwards are all rejected.
	for _, off := range []uint64{9, 6, 5, 3} {
		if _, err := seg.Append(segRecord(off)); !errors.Is(err, ErrSegmentOffsetOutOfOrder) {
			t.Errorf("Append(offset %d): err = %v, want ErrSegmentOffsetOutOfOrder", off, err)
		}
	}

	// A rejected record must not be written at all.
	if got := segFileSize(t, dir, ".log"); got != logSize {
		t.Errorf(".log size after rejected appends = %d, want %d (unchanged)", got, logSize)
	}
	if got := seg.NextOffset(); got != 7 {
		t.Errorf("NextOffset() after rejected appends = %d, want 7", got)
	}
	appendSegRecords(t, seg, 7, 7) // the correct next offset still works
}

func TestSegmentIsFull(t *testing.T) {
	seg := newTestSegment(t, t.TempDir(), 90) // each record is 45 bytes

	appendSegRecords(t, seg, 4, 4)
	if seg.IsFull() {
		t.Fatal("IsFull() = true after 45 of 90 bytes")
	}
	appendSegRecords(t, seg, 5, 5)
	if !seg.IsFull() {
		t.Fatal("IsFull() = false after 90 of 90 bytes")
	}
}

func TestSegmentReopenContinues(t *testing.T) {
	dir := t.TempDir()
	seg, err := NewSegment(dir, bigSegment, segBase)
	if err != nil {
		t.Fatal(err)
	}
	appendSegRecords(t, seg, 4, 6)
	if err := seg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newTestSegment(t, dir, bigSegment)
	if got := reopened.NextOffset(); got != 7 {
		t.Fatalf("NextOffset() after reopen = %d, want 7", got)
	}
	checkSegRecord(t, reopened, 5)
	appendSegRecords(t, reopened, 7, 8)
	for off := uint64(4); off <= 8; off++ {
		checkSegRecord(t, reopened, off)
	}
}

func TestSegmentRemoveDeletesFiles(t *testing.T) {
	dir := t.TempDir()
	seg, err := NewSegment(dir, bigSegment, segBase)
	if err != nil {
		t.Fatal(err)
	}
	appendSegRecords(t, seg, 4, 5)

	if err := seg.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, ext := range []string{".log", ".index"} {
		_, err := os.Stat(filepath.Join(dir, segmentName(segBase, ext)))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Remove (stat err = %v)", ext, err)
		}
	}
}

func TestSegmentSync(t *testing.T) {
	seg := newTestSegment(t, t.TempDir(), bigSegment)
	appendSegRecords(t, seg, 4, 6)
	if err := seg.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}
