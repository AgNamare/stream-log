package log

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const entryWidth = 8 // 4 bytes relative offset + 4 bytes position

// Index maps relative offsets to byte positions in a segment's .log file.
// It is not safe for concurrent writes; the segment is expected to lock around it.
type Index struct {
	file          *os.File
	entriesInFile uint32
}

var (
	InvalidEntryError = errors.New("Invalid Entry Number For Index File")
	ErrIndexCorrupt   = errors.New("index: corrupt entry")
)

func NewIndex(filepath string) (*Index, error) {
	file, err := os.OpenFile(filepath, os.O_RDWR|os.O_CREATE, 0644)

	if err != nil {
		return nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	size := info.Size()
	if rem := size % entryWidth; rem != 0 {
		size -= rem
		if err := file.Truncate(size); err != nil {
			file.Close()
			return nil, err
		}
	}

	entries := uint32(size / 8)

	index := &Index{
		file:          file,
		entriesInFile: entries,
	}
	return index, nil
}

func (ind *Index) Write(relOffset, pos uint32) error {
	if relOffset != ind.entriesInFile {
		return InvalidEntryError
	}

	index_buffer := make([]byte, 8)

	binary.BigEndian.PutUint32(index_buffer[0:4], relOffset)
	binary.BigEndian.PutUint32(index_buffer[4:8], pos)

	at := int64(relOffset) * entryWidth
	if _, err := ind.file.WriteAt(index_buffer[:], at); err != nil {
		_ = ind.file.Truncate(at) // drop any partially written entry
		return fmt.Errorf("index: write entry %d: %w", relOffset, err)
	}

	ind.entriesInFile += 1

	return nil
}

func (ind *Index) Read(relOffset uint32) (uint32, error) {
	if relOffset >= ind.entriesInFile {
		return 0, InvalidEntryError
	}

	var buff [entryWidth]byte

	if _, err := ind.file.ReadAt(buff[:], int64(relOffset)*entryWidth); err != nil {
		return 0, err
	}

	if buffOffset := binary.BigEndian.Uint32(buff[:4]); buffOffset != relOffset {
		return 0, ErrIndexCorrupt
	}
	pos := binary.BigEndian.Uint32(buff[4:])

	return pos, nil
}

func (ind *Index) Reset() error {
	if err := ind.file.Truncate(0); err != nil {
		return err
	}
	ind.entriesInFile = 0
	return nil
}

func (ind *Index) Entries() uint32 { return ind.entriesInFile }
func (ind *Index) Sync() error     { return ind.file.Sync() }
func (ind *Index) Close() error    { return ind.file.Close() }
