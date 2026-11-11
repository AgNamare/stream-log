package log

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const (
	lenWidth          = 4       // every record starts with a 4-byte length
	maxStoreRecordLen = 1 << 24 // 16 MiB: anything larger is treated as corruption
)

var (
	ErrStorePositionOutOfRange = errors.New("store: position out of range")
	ErrStoreIncompleteRecord   = errors.New("store: incomplete record")
	ErrStoreRecordTooLarge     = errors.New("store: record length too large")
)

// Store manages a segment's .log file. It only deals in bytes and positions;
// encoding and decoding records is done by the caller.
// It is not safe for concurrent writes; the segment is expected to lock around it.
type Store struct {
	file *os.File
	size int64 // current file size, which is also where the next record is written
}

func NewStore(path string) (*Store, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	return &Store{file: file, size: info.Size()}, nil
}

// Append writes an encoded record at the end of the file and returns the
// position it was written at.
func (s *Store) Append(data []byte) (int64, error) {
	pos := s.size

	if _, err := s.file.WriteAt(data, pos); err != nil {
		_ = s.file.Truncate(pos) // drop any partially written record
		return 0, fmt.Errorf("store: append at %d: %w", pos, err)
	}

	s.size += int64(len(data))
	return pos, nil
}

// Read returns the full encoded record (including its length prefix) that
// starts at pos.
func (s *Store) Read(pos int64) ([]byte, error) {
	if pos < 0 || pos+lenWidth > s.size {
		return nil, ErrStorePositionOutOfRange
	}

	var lenBuf [lenWidth]byte
	if _, err := s.file.ReadAt(lenBuf[:], pos); err != nil {
		return nil, fmt.Errorf("store: read length at %d: %w", pos, err)
	}

	length := int64(binary.BigEndian.Uint32(lenBuf[:]))
	if length > maxStoreRecordLen {
		return nil, ErrStoreRecordTooLarge
	}
	if pos+lenWidth+length > s.size {
		return nil, ErrStoreIncompleteRecord
	}

	buf := make([]byte, lenWidth+length)
	if _, err := s.file.ReadAt(buf, pos); err != nil {
		return nil, fmt.Errorf("store: read record at %d: %w", pos, err)
	}
	return buf, nil
}

// Truncate cuts the file back to size bytes. Recovery uses it to remove a
// partially written record left by a crash.
func (s *Store) Truncate(size int64) error {
	if size < 0 || size > s.size {
		return ErrStorePositionOutOfRange
	}
	if err := s.file.Truncate(size); err != nil {
		return err
	}
	s.size = size
	return nil
}

func (s *Store) Size() int64  { return s.size }
func (s *Store) Sync() error  { return s.file.Sync() }
func (s *Store) Close() error { return s.file.Close() }
