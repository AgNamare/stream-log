package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commitlog "stream-log/internal/log"
)

// writeSegment creates a log with n records and returns the path of its
// first .log file.
func writeSegment(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	l, err := commitlog.Open(dir, commitlog.Config{MaxSegSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := l.Append([]byte("0712"), []byte("Paid 500")); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "00000000000000000000.log")
}

func runDump(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDumpValidFile(t *testing.T) {
	path := writeSegment(t, 3)
	code, out, _ := runDump(path)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	for _, want := range []string{"offset=0 ", "offset=1 ", "offset=2 ", `key="0712"`, `value="Paid 500"`, "crc=OK", "3 valid records (bytes 0 to 135 of 135)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDumpEmptyFile(t *testing.T) {
	path := writeSegment(t, 0)
	code, out, _ := runDump(path)
	if code != 0 || !strings.Contains(out, "0 valid records") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}

func TestDumpTornRecord(t *testing.T) {
	path := writeSegment(t, 3)
	if err := os.Truncate(path, 135-5); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runDump(path)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "pos=90") || !strings.Contains(out, "ERROR") || !strings.Contains(out, "2 valid records") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestDumpCorruptRecord(t *testing.T) {
	path := writeSegment(t, 3)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[50] ^= 0xFF // inside the second record
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runDump(path)
	if code != 1 || !strings.Contains(out, "1 valid records") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}

func TestDumpLimit(t *testing.T) {
	path := writeSegment(t, 3)
	code, out, _ := runDump("-limit", "2", path)
	if code != 0 || strings.Contains(out, "offset=2 ") || !strings.Contains(out, "2 valid records") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}

func TestDumpUsageErrors(t *testing.T) {
	if code, _, _ := runDump(); code != 2 {
		t.Errorf("no arguments: exit code = %d, want 2", code)
	}
	missing := filepath.Join(t.TempDir(), "missing.log")
	if code, _, _ := runDump(missing); code != 2 {
		t.Errorf("missing file: exit code = %d, want 2", code)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("logdump created the missing file; it must never create files")
	}
}