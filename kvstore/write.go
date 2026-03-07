package kvstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sort"
	"time"
)

func (db *DB) Put(key string, value string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := db.ensureOpen(); err != nil {
		return err
	}
	if err := db.mustReadWrite(); err != nil {
		return err
	}
	if value == tombstoneValue {
		return reservedValueError()
	}

	ts := time.Now().UnixNano()
	recBytes, err := encodeDataRecord(ts, key, []byte(value))
	if err != nil {
		return err
	}

	entry, err := db.appendRecordLocked(recBytes, ts)
	if err != nil {
		return err
	}
	db.keydir[key] = entry
	return nil
}

func (db *DB) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := db.ensureOpen(); err != nil {
		return err
	}
	if err := db.mustReadWrite(); err != nil {
		return err
	}

	ts := time.Now().UnixNano()
	recBytes, err := encodeDataRecord(ts, key, []byte(tombstoneValue))
	if err != nil {
		return err
	}
	if _, err := db.appendRecordLocked(recBytes, ts); err != nil {
		return err
	}
	delete(db.keydir, key)
	return nil
}

func (db *DB) appendRecordLocked(record []byte, ts int64) (KeyDirEntry, error) {
	if db.activeFile == nil {
		return KeyDirEntry{}, errors.New("no active file open")
	}

	recordLen := int64(len(record))
	if db.activeOffset+recordLen > db.opts.MaxDataFileSizeBytes && db.activeOffset > 0 {
		if err := db.rotateActiveFileLocked(); err != nil {
			return KeyDirEntry{}, err
		}
	}

	offset := db.activeOffset
	if _, err := db.activeFile.Write(record); err != nil {
		return KeyDirEntry{}, err
	}
	db.activeOffset += recordLen

	if db.opts.SyncOnPut {
		if err := db.activeFile.Sync(); err != nil {
			return KeyDirEntry{}, err
		}
	}

	return KeyDirEntry{
		FileID:     db.activeFileID,
		FileOffset: offset,
		RecordSize: uint32(recordLen),
		TimeStamp:  ts,
	}, nil
}

func (db *DB) rotateActiveFileLocked() error {
	if db.activeFile != nil {
		oldID := db.activeFileID
		if err := db.activeFile.Sync(); err != nil {
			return err
		}
		if err := db.activeFile.Close(); err != nil {
			return err
		}
		db.activeFile = nil
		if err := db.writeHintForDataFile(oldID); err != nil {
			return err
		}
	}

	newID := db.nextFileID
	if newID == 0 {
		newID = 1
	}
	if err := db.createNewActiveFile(newID); err != nil {
		return err
	}
	db.nextFileID = newID + 1
	return nil
}

func (db *DB) createNewActiveFile(fileID uint64) error {
	path := dataFilePath(db.dir, fileID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	db.activeFile = f
	db.activeFileID = fileID
	db.activeOffset = st.Size()
	return nil
}

func (db *DB) writeHintForDataFile(fileID uint64) error {
	dataPath := dataFilePath(db.dir, fileID)
	hintPath := hintFilePath(db.dir, fileID)
	tmpPath := hintPath + tmpFileSuffix

	hintFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	dataFile, err := os.Open(dataPath)
	if err != nil {
		_ = hintFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	offset := int64(0)
	for {
		rec, size, err := decodeDataRecord(dataFile)
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = dataFile.Close()
			_ = hintFile.Close()
			_ = os.Remove(tmpPath)
			return err
		}

		h := hintRecord{
			timestamp:  rec.timestamp,
			key:        rec.key,
			recordOff:  offset,
			recordSize: uint32(size),
			tombstone:  bytes.Equal(rec.value, []byte(tombstoneValue)),
		}
		if err := encodeHintRecord(hintFile, h); err != nil {
			_ = dataFile.Close()
			_ = hintFile.Close()
			_ = os.Remove(tmpPath)
			return err
		}

		offset += int64(size)
	}

	if err := dataFile.Close(); err != nil {
		_ = hintFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := hintFile.Sync(); err != nil {
		_ = hintFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := hintFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, hintPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (db *DB) allocateDataFileID() (uint64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return 0, os.ErrClosed
	}
	id := db.nextFileID
	if id == 0 {
		id = 1
	}
	db.nextFileID = id + 1
	return id, nil
}

func (db *DB) Sync() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := db.ensureOpen(); err != nil {
		return err
	}
	if err := db.mustReadWrite(); err != nil {
		return err
	}
	if db.activeFile == nil {
		return nil
	}
	return db.activeFile.Sync()
}

func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return nil
	}

	var closeErr error
	if db.activeFile != nil {
		if err := db.activeFile.Sync(); err != nil {
			closeErr = err
		}
		if err := db.activeFile.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		db.activeFile = nil
	}

	db.readersMu.Lock()
	db.closeAllReadersLocked()
	db.readersMu.Unlock()

	db.releaseWriterLock()
	db.closed = true
	return closeErr
}

func sortedKeys(m map[string]KeyDirEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
