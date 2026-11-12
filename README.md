# stream-log

A distributed, fault-tolerant message log written in Go, inspired by Apache Kafka.

Producers append messages to a log; consumers read them back by offset, at their own pace. The project is being built in phases, from a single-node storage engine up to a replicated cluster with partitioned topics, automatic failover and consumer groups.

## Status

**Phase 1 complete: the storage engine.** A segmented, append-only commit log that stores records on disk, recovers cleanly from crashes, and deletes old data by age or size.

- Binary record format with a CRC32 checksum on every record
- Log split into segments, each a `.log` file of records plus an `.index` file for fast lookup by offset
- Automatic rolling to a new segment when the active one is full
- Crash recovery: torn or corrupt records at the end of the log are detected and cut off, and the index is rebuilt to match
- Retention by age (newest record in a segment) and by total size, never deleting the active segment and never leaving gaps in offsets
- Safe for concurrent use: many readers in parallel, appends serialised by a lock
- Tested with the race detector, including simulated crashes and concurrent appends, reads and retention
- `logdump`, a command-line tool that prints and validates the records in a `.log` file

The design is described in detail in [docs/storage-engine.md](docs/storage-engine.md).

## Using the log

```go
l, err := log.Open("data/payments-0", log.Config{
	MaxSegSize:   16 << 20,           // roll to a new segment at 16 MiB
	MaxAge:       7 * 24 * time.Hour, // delete segments older than 7 days
	MaxTotalSize: 10 << 30,           // keep at most 10 GiB
})
if err != nil {
	// handle error
}
defer l.Close()

offset, err := l.Append([]byte("0712111111"), []byte("Paid KES 500"))
record, err := l.Read(offset)

removed, err := l.ApplyRetention() // delete segments past the limits
```

## Running the tests

```bash
go test -race ./...
```

The suite covers each layer separately (record, index, store, segment) and the log as a whole: rolling, reopening, crash recovery (torn records, missing index entries, corrupted bytes), retention, and concurrent appends, reads and retention under the race detector. The `logdump` tool has its own tests for valid, empty, torn and corrupt files.

## Benchmarks

```bash
go test -run '^$' -bench . -benchmem ./internal/log
```

Results on an Intel Core i5-5200U (2 cores, 4 threads), Linux, Go `<version>`, `<RAM>`, `<SSD/HDD>`:

| Operation | Time per op | Ops per second | Throughput |
|---|---|---|---|
| Append, 100 B value | 2.4 µs | ~410,000 | 45 MB/s |
| Append, 1 KB value | 3.9 µs | ~260,000 | 268 MB/s |
| Append, 10 KB value | 20 µs | ~50,000 | 508 MB/s |
| Append, 1 KB, 4 goroutines | 4.8 µs | ~210,000 | 215 MB/s |
| Read sequential, 1 KB | 6.0 µs | ~165,000 | 171 MB/s |
| Read random, 1 KB | 5.3 µs | ~190,000 | 195 MB/s |
| Read, 1 KB, 4 goroutines | 1.8 µs | ~565,000 | 583 MB/s |
| Open with recovery, 10,000 records | 69 ms | | |

Appends allocate once per record and reads twice.

**Important caveat:** appends are not fsynced individually. Records go to the operating system's page cache and reach the disk when the OS flushes them, when a segment rolls (the full segment is synced), or after recovery. A machine crash can therefore lose the most recent unsynced records; a process crash cannot. This is the same trade-off Kafka makes, relying on replication (Phase 5) rather than per-write fsync for durability.

What the numbers show:

- Parallel reads are about 3.4× faster than single reads, because readers share a read lock.
- Parallel appends are slightly slower than a single writer, because appends to one log are serialised. Write parallelism will come from partitions (Phase 3).
- Larger records give much higher throughput, since fixed per-call costs are spread over more bytes. Producer batching (Phase 3) addresses small messages.
- Startup time grows with the size of the active segment, because recovery scans it from the beginning.

## Inspecting a log file

`logdump` prints every record in a segment's `.log` file and validates it the same way crash recovery does:

```bash
go run ./cmd/logdump data/payments-0/00000000000000000000.log
```

```
pos=0        offset=0      time=2026-11-12 10:13:00.917  key="0712"  value="Paid 500"  crc=OK
pos=45       offset=1      time=2026-11-12 10:13:00.917  key="0745"  value="Paid KES 1,000"  crc=OK
pos=96       offset=2      time=2026-11-12 10:13:00.917  key=""  value="\x00\x01\xff"  crc=OK
3 valid records (bytes 0 to 132 of 132)
```

It stops at the first torn or corrupt record, or the first offset out of sequence, and reports it. Flags: `-limit N` prints only the first N records, and `-from POS` starts at a byte position. The exit code is 0 for a valid file, 1 if a problem is found, and 2 for usage errors, so it can be used in scripts. It only ever reads; it never creates or changes files.

To generate some sample data to inspect, run `go run ./cmd/loggen`, which appends five records to `data/payments-0` each time it runs. You can then simulate a crash by cutting bytes off the end of the file (`truncate -s -5 data/payments-0/00000000000000000000.log`), see `logdump` report the torn record, and run `loggen` again to watch recovery repair it.

## Project layout

```
stream-log/
├── cmd/
│   ├── logdump/         # prints and validates a .log file
│   └── loggen/          # writes sample records for trying things out
├── internal/log/        # Phase 1: the storage engine
│   ├── record.go        # binary record format: encode / decode
│   ├── store.go         # the .log file: append bytes, read at a position
│   ├── index.go         # the .index file: relative offset → position
│   ├── segment.go       # one .log + .index pair kept in step
│   ├── log.go           # list of segments, rolling, recovery, retention
│   └── *_test.go        # tests and benchmarks
└── docs/
    └── storage-engine.md
```

Data written while trying the tools goes to `data/`, which is ignored by Git.

## Roadmap

| Phase | Builds | Status |
|---|---|---|
| 1 | Storage engine: segments, index, crash recovery, retention | ✅ Done |
| 2 | Single broker over gRPC: produce, consume, streaming, CLI | Next |
| 3 | Topics, partitions, key-based partitioning, committed consumer offsets, batching | Planned |
| 4 | Multi-broker cluster with a Raft-elected controller for metadata | Planned |
| 5 | Leader–follower replication, in-sync replicas, high watermark, failover | Planned |
| 6 | Consumer groups with automatic rebalancing | Planned |
| 7 | Docker Compose cluster, metrics, full-system benchmarks, CI | Planned |

## How this project was built (AI assistance)

I used an AI assistant (Claude) throughout Phase 1 as a tutor, reviewer and test writer:

- **Design and concepts:** explaining how commit logs, segments, indexes and recovery work, and reviewing my design decisions.
- **Tests and benchmarks:** most of the test suite and all benchmarks were written by the AI as specifications, and I wrote the implementation to make them pass. I wrote the record tests myself.
- **Code review:** reviewing my code and pointing out bugs, usually as hints rather than fixes.
- **Documentation:** this README and the design document were drafted by the AI from the project and edited by me.

I wrote most of the implementation, including the record format, segments, the log's opening, reading, appending and rolling, and the binary search for segments. For some parts, the AI also provided reference implementations or fixes that I integrated: the store (`store.go`), parts of the index, index rebuilding during recovery, segment removal, and retention. The `logdump` tool and its tests, and the small `loggen` helper, were written by the AI.