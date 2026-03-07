package kvstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sort"
)

func (db *DB) Get(key string) ([]byte, error) {
	db.mu.RLock()
	if db.closed {
		db.mu.RUnlock()
		return nil, os.ErrClosed
	}
	entry, exists := db.keydir[key]
	db.mu.RUnlock()
	if !exists {
		return nil, ErrKeyNotFound
	}

	rec, err := db.readRecordAtEntry(key, entry)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(rec.value, []byte(tombstoneValue)) {
		return nil, ErrKeyNotFound
	}
	return rec.value, nil
}

func (db *DB) ListKeys() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()

	keys := make([]string, 0, len(db.keydir))
	for k := range db.keydir {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (db *DB) Keys() []string {
	return db.ListKeys()
}

func (db *DB) Fold(fn func(key string, value []byte) error) error {
	db.mu.RLock()
	if db.closed {
		db.mu.RUnlock()
		return os.ErrClosed
	}
	keys := make([]string, 0, len(db.keydir))
	for k := range db.keydir {
		keys = append(keys, k)
	}
	db.mu.RUnlock()

	sort.Strings(keys)
	for _, k := range keys {
		v, err := db.Get(k)
		if err != nil {
			if errors.Is(err, ErrKeyNotFound) {
				continue
			}
			return err
		}
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) readRecordAtEntry(key string, entry KeyDirEntry) (*decodedRecord, error) {
	f, err := db.openReader(entry.FileID)
	if err != nil {
		return nil, err
	}

	sr := io.NewSectionReader(f, entry.FileOffset, int64(entry.RecordSize))
	rec, size, err := decodeDataRecord(sr)
	if err != nil {
		if errors.Is(err, ErrCorruptData) {
			return nil, ErrCorruptData
		}
		return nil, err
	}
	if rec.key != key || uint32(size) != entry.RecordSize {
		return nil, ErrCorruptData
	}
	return &rec, nil
}

func (db *DB) openReader(fileID uint64) (*os.File, error) {
	db.readersMu.Lock()
	defer db.readersMu.Unlock()

	if db.closed {
		return nil, os.ErrClosed
	}
	if f, ok := db.readers[fileID]; ok {
		return f, nil
	}

	path := dataFilePath(db.dir, fileID)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	db.readers[fileID] = f
	return f, nil
}

func (db *DB) closeReaderLocked(fileID uint64) {
	if f, ok := db.readers[fileID]; ok {
		_ = f.Close()
		delete(db.readers, fileID)
	}
}

func (db *DB) closeAllReadersLocked() {
	for id, f := range db.readers {
		_ = f.Close()
		delete(db.readers, id)
	}
}
