// Command logdump prints the records in a segment's .log file in readable
// form and checks that every record is valid, the same way crash recovery
// does.
//
// Usage:
//
//	logdump [-from POS] [-limit N] FILE.log
//
// It exits with status 0 if every record from the starting position to the
// end of the file is valid, 1 if it finds a torn or corrupt record, and 2 for
// usage errors.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	commitlog "stream-log/internal/log"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run holds all the logic so it can be tested without starting a process.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("logdump", flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.Int64("from", 0, "byte position to start reading at")
	limit := flags.Int("limit", 0, "print at most this many records (0 means all)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: logdump [-from POS] [-limit N] FILE.log")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	path := flags.Arg(0)

	// NewStore creates missing files, so check first: this tool must never
	// create or change anything.
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(stderr, "logdump: %v\n", err)
		return 2
	}
	if !info.Mode().IsRegular() {
		fmt.Fprintf(stderr, "logdump: %s is not a regular file\n", path)
		return 2
	}

	store, err := commitlog.NewStore(path)
	if err != nil {
		fmt.Fprintf(stderr, "logdump: open %s: %v\n", path, err)
		return 2
	}
	defer store.Close()

	// If the file is named like a segment (20-digit base offset), check that
	// offsets run on from the base offset with no gaps, as recovery does.
	baseOffset, haveBase := parseBaseOffset(path)

	pos := *from
	valid := 0
	for {
		if *limit > 0 && valid >= *limit {
			break
		}

		recordBytes, err := store.Read(pos)
		if errors.Is(err, commitlog.ErrStorePositionOutOfRange) && pos == store.Size() {
			break // clean end of file
		}
		if err != nil {
			fmt.Fprintf(stdout, "pos=%-8d ERROR: %v\n", pos, err)
			printSummary(stdout, valid, pos, *from, store.Size())
			return 1
		}

		rec, err := commitlog.Decode(recordBytes)
		if err != nil {
			fmt.Fprintf(stdout, "pos=%-8d ERROR: %v\n", pos, err)
			printSummary(stdout, valid, pos, *from, store.Size())
			return 1
		}

		if haveBase && *from == 0 {
			expected := baseOffset + uint64(valid)
			if uint64(rec.Offset) != expected {
				fmt.Fprintf(stdout, "pos=%-8d ERROR: offset %d, expected %d\n", pos, rec.Offset, expected)
				printSummary(stdout, valid, pos, *from, store.Size())
				return 1
			}
		}

		fmt.Fprintf(stdout, "pos=%-8d offset=%-6d time=%s  key=%q  value=%q  crc=OK\n",
			pos,
			rec.Offset,
			time.UnixMilli(rec.Timestamp).UTC().Format("2006-01-02 15:04:05.000"),
			rec.Key,
			rec.Value,
		)

		valid++
		pos += int64(len(recordBytes))
	}

	printSummary(stdout, valid, pos, *from, store.Size())
	return 0
}

func printSummary(w io.Writer, valid int, pos, from, size int64) {
	fmt.Fprintf(w, "%d valid records (bytes %d to %d of %d)\n", valid, from, pos, size)
}

// parseBaseOffset reads the base offset from a segment filename such as
// 00000000000000002000.log.
func parseBaseOffset(path string) (uint64, bool) {
	name := strings.TrimSuffix(filepath.Base(path), ".log")
	if len(name) != 20 {
		return 0, false
	}
	base, err := strconv.ParseUint(name, 10, 64)
	if err != nil {
		return 0, false
	}
	return base, true
}