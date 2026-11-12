package log

import (
	"fmt"
	"math/rand"
	"testing"
)

// Run with:
//   go test -run '^$' -bench . -benchmem ./internal/log
//
// -run '^$' skips the normal tests so only benchmarks run.

const benchSegmentSize = 16 << 20 // 16 MiB, so rolling happens as it would in real use

func openBenchLog(b *testing.B) *Log {
	b.Helper()
	l, err := Open(b.TempDir(), Config{MaxSegSize: benchSegmentSize})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = l.Close() })
	return l
}

func benchValue(size int) []byte {
	v := make([]byte, size)
	for i := range v {
		v[i] = byte('a' + i%26)
	}
	return v
}

// fillLog appends n records of the given value size.
func fillLog(b *testing.B, l *Log, n, valueSize int) {
	b.Helper()
	key, value := []byte("0712111111"), benchValue(valueSize)
	for i := 0; i < n; i++ {
		if _, err := l.Append(key, value); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
}

var benchSizes = []int{100, 1024, 10 * 1024}

func sizeName(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%dKB", n/1024)
	}
	return fmt.Sprintf("%dB", n)
}

// BenchmarkAppend measures single-writer appends, including segment rolls.
func BenchmarkAppend(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(sizeName(size), func(b *testing.B) {
			l := openBenchLog(b)
			key, value := []byte("0712111111"), benchValue(size)

			b.SetBytes(int64(len(key) + len(value))) // reports MB/s
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := l.Append(key, value); err != nil {
					b.Fatalf("Append: %v", err)
				}
			}
		})
	}
}

// BenchmarkAppendParallel measures appends from many goroutines at once.
// Appends are serialised by the log's lock, so this shows lock overhead.
func BenchmarkAppendParallel(b *testing.B) {
	l := openBenchLog(b)
	key, value := []byte("0712111111"), benchValue(1024)

	b.SetBytes(int64(len(key) + len(value)))
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := l.Append(key, value); err != nil {
				b.Errorf("Append: %v", err)
				return
			}
		}
	})
}

const benchRecords = 10_000

// BenchmarkReadSequential reads offsets in order, like a consumer catching up.
func BenchmarkReadSequential(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(sizeName(size), func(b *testing.B) {
			l := openBenchLog(b)
			fillLog(b, l, benchRecords, size)

			b.SetBytes(int64(10 + size))
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := l.Read(uint64(i % benchRecords)); err != nil {
					b.Fatalf("Read: %v", err)
				}
			}
		})
	}
}

// BenchmarkReadRandom reads offsets in random order, like many consumers
// resuming from different positions.
func BenchmarkReadRandom(b *testing.B) {
	l := openBenchLog(b)
	fillLog(b, l, benchRecords, 1024)

	rng := rand.New(rand.NewSource(1)) // fixed seed: same order every run
	offsets := make([]uint64, 4096)
	for i := range offsets {
		offsets[i] = uint64(rng.Intn(benchRecords))
	}

	b.SetBytes(10 + 1024)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := l.Read(offsets[i%len(offsets)]); err != nil {
			b.Fatalf("Read: %v", err)
		}
	}
}

// BenchmarkReadParallel measures concurrent readers sharing the read lock.
func BenchmarkReadParallel(b *testing.B) {
	l := openBenchLog(b)
	fillLog(b, l, benchRecords, 1024)

	b.SetBytes(10 + 1024)
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := l.Read(uint64(i % benchRecords)); err != nil {
				b.Errorf("Read: %v", err)
				return
			}
			i++
		}
	})
}

// BenchmarkOpenWithRecovery measures startup time: opening a partition and
// scanning the active segment, which recovery does on every Open.
func BenchmarkOpenWithRecovery(b *testing.B) {
	for _, records := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("%drecords", records), func(b *testing.B) {
			dir := b.TempDir()
			l, err := Open(dir, Config{MaxSegSize: 1 << 30}) // one segment, so all records are scanned
			if err != nil {
				b.Fatal(err)
			}
			fillLog(b, l, records, 1024)
			if err := l.Close(); err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				l, err := Open(dir, Config{MaxSegSize: 1 << 30})
				if err != nil {
					b.Fatalf("Open: %v", err)
				}
				b.StopTimer()
				_ = l.Close()
				b.StartTimer()
			}
		})
	}
}
