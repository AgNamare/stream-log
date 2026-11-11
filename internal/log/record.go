package log

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"
)

var (
	CorruptedRecord = errors.New("Corrupted Record")
	InvalidVersion  = errors.New("InvalidRecordVersion")
	versions        = []int{1, 2}
)

type Record struct {
	Offset    int64
	Timestamp int64
	Key       []byte
	Value     []byte
}

func Encode(r Record) []byte {
	keyLen := len(r.Key)
	valueLen := len(r.Value)

	buff := make([]byte, 33+keyLen+valueLen)

	//version
	buff[8] = 1
	//offset & Timestamp
	binary.BigEndian.PutUint64(buff[9:17], uint64(r.Offset))
	binary.BigEndian.PutUint64(buff[17:25], uint64(r.Timestamp))

	//Key length & Key
	binary.BigEndian.PutUint32(buff[25:29], uint32(keyLen))
	keyEnd := 29 + keyLen
	copy(buff[29:keyEnd], r.Key)

	//Value length & Key
	binary.BigEndian.PutUint32(buff[keyEnd:keyEnd+4], uint32(valueLen))
	copy(buff[keyEnd+4:keyEnd+4+valueLen], r.Value)

	len_bytes := len(buff[4:])

	binary.BigEndian.PutUint32(buff[0:4], uint32(len_bytes))

	//checksum
	table := crc32.MakeTable(crc32.IEEE)
	checksum := crc32.Checksum(buff[8:], table)

	binary.BigEndian.PutUint32(buff[4:8], checksum)
	return buff
}

func Decode(buff []byte) (*Record, error) {
	if len(buff) <= 4 {
		return nil, CorruptedRecord
	}

	// Check length
	binary_len := buff[:4]
	recordLength := binary.BigEndian.Uint32(binary_len)

	if uint32(len(buff[4:])) != recordLength {
		return nil, CorruptedRecord
	}

	//Checksum
	table := crc32.MakeTable(crc32.IEEE)
	checksum := crc32.Checksum(buff[8:], table)
	buffChecksum := binary.BigEndian.Uint32(buff[4:8])
	if checksum != buffChecksum {
		return nil, CorruptedRecord
	}

	//version
	version := int(buff[8])
	exists := slices.Contains(versions, version)
	if !exists {
		return nil, InvalidVersion
	}

	offset := binary.BigEndian.Uint64(buff[9:17])

	timestamp := binary.BigEndian.Uint64(buff[17:25])

	keyLen := binary.BigEndian.Uint32(buff[25:29])

	key := buff[29 : 29+keyLen]

	value := buff[29+keyLen+4:]

	record := &Record{
		Offset:    int64(offset),
		Timestamp: int64(timestamp),
		Key:       key,
		Value:     value,
	}
	return record, nil
}
