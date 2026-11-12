package log

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Segment struct {
	Store      *Store
	Index      *Index
	MaxBytes   int64
	BaseOffset uint64
}

var (
	ErrSegmentOffsetOutOfRange = errors.New("segment: offset out of range")
	ErrSegmentOffsetOutOfOrder = errors.New("segment: offset out of order")
)

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err == nil {
		return !info.IsDir()
	}

	if errors.Is(err, os.ErrNotExist) {
		return false
	}

	return false
}

func NewSegment(dir string, maxbytes int64, baseOffset uint64) (*Segment, error) {
	logFileName := fmt.Sprintf("%020d.log", baseOffset)
	indexFileName := fmt.Sprintf("%020d.index", baseOffset)
	logFilePath := filepath.Join(dir, logFileName)
	indexPath := filepath.Join(dir, indexFileName)

	store, err := NewStore(logFilePath)
	if err != nil {
		return nil, errors.New("segment: Failed to instantiate store")
	}

	index, err := NewIndex(indexPath)
	if err != nil {
		store.Close()
		return nil, errors.New("segment: Failed to instantiate Index")
	}

	return &Segment{
		Store:      store,
		Index:      index,
		MaxBytes:   maxbytes,
		BaseOffset: baseOffset,
	}, nil
}

func (seg *Segment) NextOffset() uint64 {
	return seg.BaseOffset + uint64(seg.Index.Entries())
}

func (seg *Segment) Append(rec Record) (offset uint64, err error) {
	if uint64(rec.Offset) != seg.NextOffset() {
		return 0, ErrSegmentOffsetOutOfOrder
	}

	encoded_record := Encode(rec)

	pos, err := seg.Store.Append(encoded_record)

	if err != nil {
		return 0, errors.New("segment: failed to append to log file")
	}

	relOffset := rec.Offset - seg.BaseOffset

	err = seg.Index.Write(uint32(relOffset), uint32(pos))
	if err != nil {
		seg.Store.Truncate(pos)
		return 0, errors.New("segment: failed to write to index file")
	}
	return uint64(rec.Offset), nil

}

func (seg *Segment) Remove() error {
	logPath := seg.Store.file.Name()
	indexPath := seg.Index.file.Name()

	closeErr := seg.Close()

	logErr := os.Remove(logPath)
	if errors.Is(logErr, os.ErrNotExist) {
		logErr = nil
	}

	indexErr := os.Remove(indexPath)
	if errors.Is(indexErr, os.ErrNotExist) {
		indexErr = nil
	}

	return errors.Join(closeErr, logErr, indexErr)
}

func (seg *Segment) Read(offset uint64) (Record, error) {
	if offset < seg.BaseOffset || offset >= seg.NextOffset() {
		return Record{}, ErrSegmentOffsetOutOfRange
	}

	relOffset := offset - seg.BaseOffset

	pos, err := seg.Index.Read(uint32(relOffset))

	if err != nil {
		return Record{}, errors.New("segment: Failed to Read Index")
	}

	log_bytes, err := seg.Store.Read(int64(pos))

	if err != nil {
		return Record{}, errors.New("segment: Failed to fetch log bytes")
	}

	rec, err := Decode(log_bytes)
	if err != nil {
		return Record{}, errors.New("segment: Failed to decode bytes into record")
	}

	return *rec, nil
}

func (seg *Segment) IsFull() bool {
	return seg.Store.Size() >= seg.MaxBytes
}

func (seg *Segment) Sync() error {
	store_err := seg.Store.Sync()
	index_err := seg.Index.Sync()
	if store_err != nil || index_err != nil {
		return errors.New("Failed to Sync segment")
	}
	return nil
}

func (seg *Segment) rebuildIndex(positions []int64) error {
	if err := seg.Index.Reset(); err != nil {
		return fmt.Errorf("segment %d: reset index: %w", seg.BaseOffset, err)
	}

	for i, pos := range positions {
		if err := seg.Index.Write(uint32(i), uint32(pos)); err != nil {
			return fmt.Errorf("segment %d: rebuild index entry %d: %w", seg.BaseOffset, i, err)
		}
	}

	if err := seg.Index.Sync(); err != nil {
		return fmt.Errorf("segment %d: sync index: %w", seg.BaseOffset, err)
	}
	return nil
}

func (seg *Segment) Close() error {
	index_err := seg.Index.Close()
	store_err := seg.Store.Close()
	if index_err != nil || store_err != nil {
		return errors.New("segment:Failed to close segment files")
	}
	return nil
}
