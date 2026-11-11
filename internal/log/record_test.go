package log

import (
	"bytes"
	"hash/crc32"
	"testing"
)

func TestEncodeDecode_Success(t *testing.T) {
	original := Record{
		Offset:    1042,
		Timestamp: 1718000000,
		Key:       []byte("user_id"),
		Value:     []byte("auth_token_xyz123"),
	}

	encoded := Encode(original)
	decoded, err := Decode(encoded)

	if err != nil {
		t.Fatalf("unexpected error during decode: %v", err)
	}

	if decoded.Offset != original.Offset {
		t.Errorf("expected offset %d, got %d", original.Offset, decoded.Offset)
	}

	if decoded.Timestamp != original.Timestamp {
		t.Errorf("expected timestamp %d, got %d", original.Timestamp, decoded.Timestamp)
	}

	if !bytes.Equal(decoded.Key, original.Key) {
		t.Errorf("expected key %s, got %s", original.Key, decoded.Key)
	}

	if !bytes.Equal(decoded.Value, original.Value) {
		t.Errorf("expected value %s, got %s", original.Value, decoded.Value)
	}
}

func TestDecode_ShortBuffer(t *testing.T) {
	// Buffer too short to contain length prefix
	shortBuff := []byte{0x00, 0x01}

	_, err := Decode(shortBuff)
	if err != CorruptedRecord {
		t.Errorf("expected CorruptedRecord for short buffer, got %v", err)
	}
}

func TestDecode_CorruptedRecord(t *testing.T) {
	original := Record{
		Offset:    1,
		Timestamp: 123456789,
		Key:       []byte("key"),
		Value:     []byte("value"),
	}

	encoded := Encode(original)

	// Tamper with value bytes to invalidate the checksum
	encoded[len(encoded)-1] ^= 0xFF

	_, err := Decode(encoded)
	if err != CorruptedRecord {
		t.Errorf("expected CorruptedRecord due to checksum mismatch, got %v", err)
	}
}

func TestDecode_InvalidVersion(t *testing.T) {
	original := Record{
		Offset:    1,
		Timestamp: 123456789,
		Key:       []byte("key"),
		Value:     []byte("value"),
	}

	encoded := Encode(original)

	// Set version byte (index 8) to an unsupported version (e.g., 99)
	encoded[8] = 99

	// Recalculate checksum so it passes the checksum check and reaches version validation
	table := crc32.MakeTable(crc32.IEEE)
	checksum := crc32.Checksum(encoded[8:], table)
	binaryBigEndianPutUint32TestHelper(encoded[4:8], checksum)

	_, err := Decode(encoded)
	if err != InvalidVersion {
		t.Errorf("expected InvalidVersion error, got %v", err)
	}
}

// Helper to avoid duplicate import of encoding/binary if needed in test file
func binaryBigEndianPutUint32TestHelper(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
