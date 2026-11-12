package log

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	MaxSegSize   int64
	MaxAge       time.Duration
	MaxTotalSize int64
	Now          func() time.Time
}

type Log struct {
	FolderPath string
	Cf         Config
	Segments   []*Segment
	Mu         sync.RWMutex
}

var ErrOffsetOutOfRange = errors.New("log: offset out of range")
var ErrLogEmpty = errors.New("log: no records yet")

func Open(dir string, cf Config) (*Log, error) {
	if cf.MaxSegSize <= 0 {
		return nil, errors.New("log: Segment size not configured")
	}
	log := &Log{
		FolderPath: dir,
		Cf:         cf,
	}
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return nil, errors.New("log: failed to create log directory")
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New("log: failed to read directory")
	}

	var baseOffsets []int
	var segments []*Segment

	//extract base offsets
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		file, err := file.Info()
		if err != nil {
			continue
		}
		fileExt := filepath.Ext(file.Name())
		fileName := strings.TrimSuffix(file.Name(), fileExt)
		if fileExt == ".log" {
			baseOffset, err := strconv.ParseUint(fileName, 10, 64)
			if err != nil {
				continue
			}
			baseOffsets = append(baseOffsets, int(baseOffset))
		}
	}
	slices.Sort(baseOffsets)

	//initialize segments and add them to segments slice
	if offset_len := len(baseOffsets); offset_len == 0 {
		seg, err := NewSegment(dir, cf.MaxSegSize, 0)
		if err != nil {
			return nil, errors.New("Failed to create the first base segment")
		}
		segments = append(segments, seg)
	}
	for _, offset := range baseOffsets {
		seg, err := NewSegment(dir, cf.MaxSegSize, uint64(offset))
		if err != nil {
			for _, reg := range segments {
				reg.Close()
			}
			return nil, errors.New("log: segment failed to initialize")
		}
		segments = append(segments, seg)
	}

	log.Segments = segments

	activeSegment := log.ActiveSegment()
	if err := recovery(activeSegment); err != nil {
		for _, seg := range log.Segments {
			seg.Close()
		}
		return nil, err
	}

	return log, nil
}

// Recover the from the active segment if there was record with an error or
// Failed write attempt onto disk
func recovery(activeSegment *Segment) error {
	pos := 0
	var relOffset int
	var positions []int64
	for {
		record_bytes, err := activeSegment.Store.Read(int64(pos))
		if err != nil {
			// These mean "valid data ends here": the normal end of the file,
			// a torn final record, or a corrupt length field.
			if errors.Is(err, ErrStorePositionOutOfRange) ||
				errors.Is(err, ErrStoreIncompleteRecord) ||
				errors.Is(err, ErrStoreRecordTooLarge) {
				break
			}
			// Anything else is a real disk error. Return it so Open fails
			// without truncating records that may be perfectly good.
			return fmt.Errorf("log: recover segment %d: %w", activeSegment.BaseOffset, err)
		}
		if len(record_bytes[4:]) != int(binary.BigEndian.Uint32(record_bytes[0:4])) {
			break
		}

		if _, err := Decode(record_bytes); err != nil {
			break
		}

		file_offset := binary.BigEndian.Uint64(record_bytes[9:17])
		expected_offset := activeSegment.BaseOffset + uint64(relOffset)
		if file_offset != expected_offset {
			break
		}

		positions = append(positions, int64(pos))
		pos = pos + len(record_bytes)
		relOffset += 1
	}

	if err := activeSegment.Store.Truncate(int64(pos)); err != nil {
		return err
	}
	if err := activeSegment.Store.Sync(); err != nil {
		return err
	}
	return activeSegment.rebuildIndex(positions)
}

func (log *Log) ActiveSegment() *Segment {
	if len(log.Segments) > 0 {
		return log.Segments[len(log.Segments)-1]
	}
	return nil
}

func findSegment(segments []*Segment, offset uint64) (*Segment, error) {
	if len(segments) == 0 || offset < segments[0].BaseOffset {
		return nil, ErrOffsetOutOfRange
	}

	//Optimize search using binary search
	lo, hi := 0, len(segments)-1
	best := 0
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if segments[mid].BaseOffset <= offset {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return segments[best], nil
}

func (log *Log) Append(key, value []byte) (uint64, error) {
	log.Mu.Lock()
	defer log.Mu.Unlock()
	activeSegment := log.ActiveSegment()

	var appendSegment *Segment
	//check if the active segment is full and add it if not
	if activeSegment.IsFull() {
		//sync prev segment b4 creating another one
		err := activeSegment.Sync()
		if err != nil {
			return 0, err
		}
		newBaseOffset := activeSegment.NextOffset()
		newSegment, err := NewSegment(log.FolderPath, log.Cf.MaxSegSize, newBaseOffset)
		if err != nil {
			return 0, err
		}
		log.Segments = append(log.Segments, newSegment)
		appendSegment = newSegment
	} else {
		appendSegment = activeSegment
	}

	rec := Record{
		Offset:    appendSegment.NextOffset(),
		Timestamp: log.now().UnixMilli(),
		Key:       key,
		Value:     value,
	}
	offset, err := appendSegment.Append(rec)
	if err != nil {
		return 0, err
	}

	return offset, nil
}

func (log *Log) Read(offset uint64) (rec Record, err error) {
	log.Mu.RLock()
	defer log.Mu.RUnlock()

	if offset >= log.ActiveSegment().NextOffset() {
		return Record{}, ErrOffsetOutOfRange
	}

	segment, err := findSegment(log.Segments, offset)
	if err != nil {
		return Record{}, err
	}
	return segment.Read(offset)
}

func (log *Log) LowestOffset() uint64 {
	log.Mu.RLock()
	defer log.Mu.RUnlock()
	if len(log.Segments) > 0 {
		return log.Segments[0].BaseOffset
	}
	return 0
}

func (log *Log) HighestOffset() (uint64, error) {
	log.Mu.RLock()
	defer log.Mu.RUnlock()

	next := log.ActiveSegment().NextOffset()
	if next == log.Segments[0].BaseOffset {
		return 0, ErrLogEmpty
	}
	return next - 1, nil
}

func (log *Log) now() time.Time {
	if log.Cf.Now != nil {
		return log.Cf.Now()
	}
	return time.Now()
}

func (log *Log) Close() error {
	log.Mu.Lock()
	defer log.Mu.Unlock()

	var errs []error
	for _, segment := range log.Segments {
		if err := segment.Close(); err != nil {
			errs = append(errs, fmt.Errorf("log: close segment %d: %w", segment.BaseOffset, err))
		}
	}
	return errors.Join(errs...)
}

func (log *Log) ApplyRetention() (int, error) {
	log.Mu.Lock()
	defer log.Mu.Unlock()

	var errs []error
	removed := 0

	// Rule 1: age. Delete closed segments whose newest record is too old.
	if log.Cf.MaxAge > 0 {
		for i := 0; i < len(log.Segments)-1; i++ {
			segment := log.Segments[i]

			entries := segment.Index.Entries()
			if entries == 0 {
				break
			}

			lastRecordPos, err := segment.Index.Read(entries - 1)
			if err != nil {
				errs = append(errs, fmt.Errorf("log: retention: segment %d: %w", segment.BaseOffset, err))
				break
			}
			recordBytes, err := segment.Store.Read(int64(lastRecordPos))
			if err != nil {
				errs = append(errs, fmt.Errorf("log: retention: segment %d: %w", segment.BaseOffset, err))
				break
			}
			record, err := Decode(recordBytes)
			if err != nil {
				errs = append(errs, fmt.Errorf("log: retention: segment %d: %w", segment.BaseOffset, err))
				break
			}

			if log.now().Sub(time.UnixMilli(record.Timestamp)) < log.Cf.MaxAge {
				break
			}

			if err := segment.Remove(); err != nil {
				errs = append(errs, fmt.Errorf("log: retention: remove segment %d: %w", segment.BaseOffset, err))
				break
			}
			removed++
		}
	}

	// Rule 2: size. While the remaining segments are over the limit, delete
	// the oldest closed one. Starts where the age rule stopped.
	if log.Cf.MaxTotalSize > 0 && len(errs) == 0 {
		var total int64
		for _, segment := range log.Segments[removed:] {
			total += segment.Store.Size()
		}

		for total > log.Cf.MaxTotalSize && removed < len(log.Segments)-1 {
			segment := log.Segments[removed]
			size := segment.Store.Size()

			if err := segment.Remove(); err != nil {
				errs = append(errs, fmt.Errorf("log: retention: remove segment %d: %w", segment.BaseOffset, err))
				break
			}
			total -= size
			removed++
		}
	}

	// Both rules only remove from the front, so the first `removed`
	// segments are exactly the ones deleted.
	if removed > 0 {
		clear(log.Segments[:removed])
		log.Segments = log.Segments[removed:]
	}

	return removed, errors.Join(errs...)
}
