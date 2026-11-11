package log

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type Config struct {
	MaxSegSize int64
}

type Log struct {
	FolderPath string
	Cf         Config
	Segments   []*Segment
	Mu         sync.RWMutex
}

func Open(dir string, cf Config) (*Log, error) {
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

	activeSegment := activeSegmet(segments)
	if err := recovery(activeSegment); err != nil {
		return nil, err
	}

	return log, nil
}

func recovery(activeSegment *Segment) error {
	pos := 0
	var relOffset int
	var positions []int64
	for {
		//check len in file matches the len stored by the length field inside file
		record_bytes, err := activeSegment.Store.Read(int64(pos))
		if err != nil {
			break
		}
		if len(record_bytes[4:]) != int(binary.BigEndian.Uint32(record_bytes[0:4])) {
			break
		}

		//Decode
		if _, err := Decode(record_bytes); err != nil {
			break
		}

		//Check Offset Validity
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

func activeSegmet(segments []*Segment) *Segment {
	if len(segments) > 0 {
		return segments[len(segments)-1]
	}
	return nil
}
