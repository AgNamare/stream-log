package log

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeClock lets tests control "now", so records can be made days old
// without waiting. Pass clock.Now as Config.Now.
type fakeClock struct {
	mu   sync.Mutex
	base time.Time
	now  time.Time
}

func newFakeClock() *fakeClock {
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	return &fakeClock{base: base, now: base}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// SetDay moves the clock to `days` days after the start (fractions allowed).
func (c *fakeClock) SetDay(days float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.base.Add(time.Duration(days * float64(24*time.Hour)))
}

const day = 24 * time.Hour

func openRetentionLog(t *testing.T, dir string, cfg Config) *Log {
	t.Helper()
	if cfg.MaxSegSize == 0 {
		cfg.MaxSegSize = twoPerSegment // a segment holds 2 test records
	}
	l, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func appendAt(t *testing.T, l *Log, clock *fakeClock, days float64, n int) {
	t.Helper()
	clock.SetDay(days)
	for i := 0; i < n; i++ {
		if _, err := l.Append(testKey, testValue); err != nil {
			t.Fatalf("Append at day %v: %v", days, err)
		}
	}
}

func expectRemoved(t *testing.T, l *Log, want int) {
	t.Helper()
	got, err := l.ApplyRetention()
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if got != want {
		t.Fatalf("ApplyRetention removed %d segments, want %d", got, want)
	}
}

func expectGone(t *testing.T, l *Log, from, to uint64) {
	t.Helper()
	for off := from; off <= to; off++ {
		if _, err := l.Read(off); !errors.Is(err, ErrOffsetOutOfRange) {
			t.Errorf("Read(%d) of deleted data: err = %v, want ErrOffsetOutOfRange", off, err)
		}
	}
}

func expectFiles(t *testing.T, dir string, base uint64, exist bool) {
	t.Helper()
	for _, ext := range []string{".log", ".index"} {
		_, err := os.Stat(filepath.Join(dir, segmentName(base, ext)))
		if exist && err != nil {
			t.Errorf("segment %d %s should exist: %v", base, ext, err)
		}
		if !exist && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("segment %d %s should be deleted (stat err = %v)", base, ext, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. Nothing configured
// ---------------------------------------------------------------------------

func TestRetentionDisabledWhenNoLimitsSet(t *testing.T) {
	clock := newFakeClock()
	l := openRetentionLog(t, t.TempDir(), Config{Now: clock.Now})
	appendAt(t, l, clock, 0, 7) // segments 0, 2, 4, 6

	clock.SetDay(365)
	expectRemoved(t, l, 0)
	readAll(t, l, 0, 6)
}

// ---------------------------------------------------------------------------
// 2. Retention by age
// ---------------------------------------------------------------------------

func TestRetentionByAgeDeletesOldSegments(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	l := openRetentionLog(t, dir, Config{MaxAge: 7 * day, Now: clock.Now})

	appendAt(t, l, clock, 0, 2)   // segment 0: offsets 0-1, newest at day 0
	appendAt(t, l, clock, 0.5, 2) // segment 2: offsets 2-3, newest at day 0.5
	appendAt(t, l, clock, 3, 2)   // segment 4: offsets 4-5, newest at day 3
	appendAt(t, l, clock, 8, 1)   // segment 6: offset 6 (active)

	// At day 8, segments 0 and 2 are older than 7 days; segment 4 is 5 days old.
	expectRemoved(t, l, 2)

	if got := l.LowestOffset(); got != 4 {
		t.Errorf("LowestOffset() = %d, want 4", got)
	}
	expectGone(t, l, 0, 3)
	readAll(t, l, 4, 6)
	expectFiles(t, dir, 0, false)
	expectFiles(t, dir, 2, false)
	expectFiles(t, dir, 4, true)

	if high, err := l.HighestOffset(); err != nil || high != 6 {
		t.Errorf("HighestOffset() = %d, %v; want 6, nil", high, err)
	}
}

func TestRetentionByAgeUsesNewestRecordInSegment(t *testing.T) {
	clock := newFakeClock()
	l := openRetentionLog(t, t.TempDir(), Config{MaxAge: 7 * day, Now: clock.Now})

	appendAt(t, l, clock, 0, 1)   // segment 0, first record: day 0
	appendAt(t, l, clock, 6.5, 1) // segment 0, newest record: day 6.5
	appendAt(t, l, clock, 7, 1)   // segment 2 (active)

	// At day 8 the oldest record in segment 0 is 8 days old, but its newest is
	// only 1.5 days old, so the segment must be kept.
	clock.SetDay(8)
	expectRemoved(t, l, 0)
	readAll(t, l, 0, 2)
}

func TestRetentionByAgeNeverDeletesActiveSegment(t *testing.T) {
	clock := newFakeClock()
	l := openRetentionLog(t, t.TempDir(), Config{MaxAge: 7 * day, Now: clock.Now})
	appendAt(t, l, clock, 0, 3) // segment 0: offsets 0-1, segment 2 (active): offset 2

	clock.SetDay(30) // everything is very old
	expectRemoved(t, l, 1)

	readAll(t, l, 2, 2)
	if got := l.LowestOffset(); got != 2 {
		t.Errorf("LowestOffset() = %d, want 2", got)
	}

	// Appends continue in the surviving active segment.
	off, err := l.Append(testKey, testValue)
	if err != nil || off != 3 {
		t.Fatalf("Append after retention = %d, %v; want 3, nil", off, err)
	}
}

func TestRetentionByAgeStopsAtFirstKeptSegment(t *testing.T) {
	clock := newFakeClock()
	l := openRetentionLog(t, t.TempDir(), Config{MaxAge: 7 * day, Now: clock.Now})

	// The system clock jumped backwards: segment 2's records are older than
	// segment 0's. Deleting segment 2 while keeping segment 0 would leave a
	// gap in the offsets, so nothing may be deleted.
	appendAt(t, l, clock, 5, 2) // segment 0, newest at day 5 (young)
	appendAt(t, l, clock, 0, 2) // segment 2, newest at day 0 (old)
	appendAt(t, l, clock, 6, 1) // segment 4 (active)

	clock.SetDay(9)
	expectRemoved(t, l, 0)
	readAll(t, l, 0, 4)
}

func TestRetentionByAgeRunsRepeatedly(t *testing.T) {
	clock := newFakeClock()
	l := openRetentionLog(t, t.TempDir(), Config{MaxAge: 7 * day, Now: clock.Now})

	appendAt(t, l, clock, 0, 2) // segment 0
	appendAt(t, l, clock, 2, 2) // segment 2
	appendAt(t, l, clock, 4, 1) // segment 4 (active)

	clock.SetDay(8) // only segment 0 is old enough
	expectRemoved(t, l, 1)
	expectRemoved(t, l, 0) // running again changes nothing

	clock.SetDay(10) // now segment 2 is old enough too
	expectRemoved(t, l, 1)
	if got := l.LowestOffset(); got != 4 {
		t.Errorf("LowestOffset() = %d, want 4", got)
	}
}

func TestRetentionSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	cfg := Config{MaxSegSize: twoPerSegment, MaxAge: 7 * day, Now: clock.Now}

	l, err := Open(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	appendAt(t, l, clock, 0, 4) // segments 0 and 2
	appendAt(t, l, clock, 8, 1) // segment 4 (active)
	expectRemoved(t, l, 2)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	l = openRetentionLog(t, dir, cfg)
	if got := l.LowestOffset(); got != 4 {
		t.Errorf("LowestOffset() after reopen = %d, want 4", got)
	}
	expectGone(t, l, 0, 3)
	readAll(t, l, 4, 4)
}

// ---------------------------------------------------------------------------
// 3. Retention by total size
// ---------------------------------------------------------------------------

// Each closed segment here holds 2 records = 90 bytes of .log; total size is
// the sum of every segment's .log size.

func TestRetentionBySizeDeletesOldestUntilUnderLimit(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	l := openRetentionLog(t, dir, Config{MaxTotalSize: 200, Now: clock.Now})

	appendAt(t, l, clock, 0, 7) // segments 0, 2, 4 (90 bytes each) + 6 (45 bytes) = 315

	// Deleting segment 0 leaves 225 (> 200); deleting segment 2 leaves 135.
	expectRemoved(t, l, 2)
	if got := l.LowestOffset(); got != 4 {
		t.Errorf("LowestOffset() = %d, want 4", got)
	}
	expectGone(t, l, 0, 3)
	readAll(t, l, 4, 6)
	expectFiles(t, dir, 0, false)
	expectFiles(t, dir, 2, false)
}

func TestRetentionBySizeKeepsEverythingUnderLimit(t *testing.T) {
	clock := newFakeClock()
	l := openRetentionLog(t, t.TempDir(), Config{MaxTotalSize: 1000, Now: clock.Now})
	appendAt(t, l, clock, 0, 7)

	expectRemoved(t, l, 0)
	readAll(t, l, 0, 6)
}

func TestRetentionBySizeNeverDeletesActiveSegment(t *testing.T) {
	clock := newFakeClock()
	l := openRetentionLog(t, t.TempDir(), Config{MaxTotalSize: 10, Now: clock.Now})
	appendAt(t, l, clock, 0, 7) // segment 6 (active) alone is over the limit

	expectRemoved(t, l, 3) // 0, 2 and 4; the active segment stays
	readAll(t, l, 6, 6)
	if got := l.LowestOffset(); got != 6 {
		t.Errorf("LowestOffset() = %d, want 6", got)
	}
}

// ---------------------------------------------------------------------------
// 4. Concurrency (run with: go test -race)
// ---------------------------------------------------------------------------

func TestRetentionConcurrentWithAppendsAndReads(t *testing.T) {
	clock := newFakeClock()
	l := openRetentionLog(t, t.TempDir(), Config{MaxTotalSize: 450, Now: clock.Now})

	const writers, perWriter = 4, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := l.Append(testKey, testValue); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}

	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { // retention runs over and over while appends happen
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := l.ApplyRetention(); err != nil {
				t.Errorf("ApplyRetention: %v", err)
				return
			}
		}
	}()
	go func() { // a reader that keeps reading the newest offset
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if high, err := l.HighestOffset(); err == nil {
				if _, err := l.Read(high); err != nil {
					// Retention may legitimately delete the offset between the
					// two calls; any other failure is a bug.
					if errors.Is(err, ErrOffsetOutOfRange) && l.LowestOffset() > high {
						continue
					}
					t.Errorf("Read(%d): %v", high, err)
					return
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	bg.Wait()

	// Whatever survived must be one unbroken run ending at the last offset.
	if _, err := l.ApplyRetention(); err != nil {
		t.Fatalf("final ApplyRetention: %v", err)
	}
	high, err := l.HighestOffset()
	if err != nil || high != writers*perWriter-1 {
		t.Fatalf("HighestOffset() = %d, %v; want %d, nil", high, err, writers*perWriter-1)
	}
	low := l.LowestOffset()
	if low == 0 {
		t.Fatalf("LowestOffset() = 0; size retention never deleted anything")
	}
	readAll(t, l, low, high)
}