package kvstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

func decodeDataRecord(r io.Reader) (decodedRecord, int, error) {
	var crcBuf [4]byte
	n, err := io.ReadFull(r, crcBuf[:])
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if n == 0 {
				return decodedRecord{}, 0, io.EOF
			}
		}
		return decodedRecord{}, 0, err
	}
	storedCRC := binary.LittleEndian.Uint32(crcBuf[:])

	var metaBuf [16]byte
	if _, err := io.ReadFull(r, metaBuf[:]); err != nil {
		return decodedRecord{}, 0, err
	}
	timestamp := int64(binary.LittleEndian.Uint64(metaBuf[0:8]))
	keySize := binary.LittleEndian.Uint32(metaBuf[8:12])
	valueSize := binary.LittleEndian.Uint32(metaBuf[12:16])

	key := make([]byte, keySize)
	if _, err := io.ReadFull(r, key); err != nil {
		return decodedRecord{}, 0, err
	}
	value := make([]byte, valueSize)
	if _, err := io.ReadFull(r, value); err != nil {
		return decodedRecord{}, 0, err
	}

	// Recompute CRC from the serialized payload to reject torn/corrupted records.
	check := bytes.NewBuffer(make([]byte, 0, len(metaBuf)+len(key)+len(value)))
	check.Write(metaBuf[:])
	check.Write(key)
	check.Write(value)
	if crc32.ChecksumIEEE(check.Bytes()) != storedCRC {
		return decodedRecord{}, 0, ErrCorruptData
	}

	totalSize := 4 + 16 + int(keySize) + int(valueSize)
	return decodedRecord{timestamp: timestamp, key: string(key), value: value}, totalSize, nil
}

func encodeDataRecord(timestamp int64, key string, value []byte) ([]byte, error) {
	keySize := uint32(len(key))
	valueSize := uint32(len(value))

	payload := bytes.NewBuffer(make([]byte, 0, 16+len(key)+len(value)))
	if err := binary.Write(payload, binary.LittleEndian, uint64(timestamp)); err != nil {
		return nil, err
	}
	if err := binary.Write(payload, binary.LittleEndian, keySize); err != nil {
		return nil, err
	}
	if err := binary.Write(payload, binary.LittleEndian, valueSize); err != nil {
		return nil, err
	}
	if _, err := payload.Write([]byte(key)); err != nil {
		return nil, err
	}
	if _, err := payload.Write(value); err != nil {
		return nil, err
	}

	payloadBytes := payload.Bytes()
	crc := crc32.ChecksumIEEE(payloadBytes)
	record := make([]byte, 4+len(payloadBytes))
	binary.LittleEndian.PutUint32(record[0:4], crc)
	copy(record[4:], payloadBytes)
	return record, nil
}

func encodeHintRecord(w io.Writer, hr hintRecord) error {
	keyBytes := []byte(hr.key)
	if err := binary.Write(w, binary.LittleEndian, uint64(hr.timestamp)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint64(hr.recordOff)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, hr.recordSize); err != nil {
		return err
	}
	var tombstone byte
	if hr.tombstone {
		tombstone = 1
	}
	if err := binary.Write(w, binary.LittleEndian, tombstone); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(keyBytes))); err != nil {
		return err
	}
	if _, err := w.Write(keyBytes); err != nil {
		return err
	}
	return nil
}

func decodeHintRecord(r io.Reader) (hintRecord, int, error) {
	var ts uint64
	if err := binary.Read(r, binary.LittleEndian, &ts); err != nil {
		if errors.Is(err, io.EOF) {
			return hintRecord{}, 0, io.EOF
		}
		return hintRecord{}, 0, err
	}
	var off uint64
	if err := binary.Read(r, binary.LittleEndian, &off); err != nil {
		return hintRecord{}, 0, err
	}
	var recSize uint32
	if err := binary.Read(r, binary.LittleEndian, &recSize); err != nil {
		return hintRecord{}, 0, err
	}
	var tombstone byte
	if err := binary.Read(r, binary.LittleEndian, &tombstone); err != nil {
		return hintRecord{}, 0, err
	}
	var keySize uint32
	if err := binary.Read(r, binary.LittleEndian, &keySize); err != nil {
		return hintRecord{}, 0, err
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(r, key); err != nil {
		return hintRecord{}, 0, err
	}

	total := 8 + 8 + 4 + 1 + 4 + int(keySize)
	return hintRecord{
		timestamp:  int64(ts),
		key:        string(key),
		recordOff:  int64(off),
		recordSize: recSize,
		tombstone:  tombstone == 1,
	}, total, nil
}
