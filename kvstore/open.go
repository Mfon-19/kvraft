package kvstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func Open(dir string) (*DB, error) {
	return OpenWithOptions(dir, OpenOptions{ReadWrite: true})
}

func OpenReadOnly(dir string) (*DB, error) {
	return OpenWithOptions(dir, OpenOptions{ReadWrite: false})
}

func OpenWithOptions(dir string, opts OpenOptions) (*DB, error) {
	if opts.MaxDataFileSizeBytes <= 0 {
		opts.MaxDataFileSizeBytes = defaultMaxDataFileSize
	}

	if opts.ReadWrite {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	} else {
		if _, err := os.Stat(dir); err != nil {
			return nil, err
		}
	}

	db := &DB{
		dir:     dir,
		opts:    opts,
		keydir:  make(map[string]KeyDirEntry),
		readers: make(map[uint64]*os.File),
	}

	if err := db.validateLayout(); err != nil {
		return nil, err
	}

	if opts.ReadWrite {
		if err := db.acquireWriterLock(); err != nil {
			return nil, err
		}
	}

	ids, err := listDataFileIDs(dir)
	if err != nil {
		db.releaseWriterLock()
		return nil, err
	}

	if len(ids) == 0 {
		if opts.ReadWrite {
			if err := db.createNewActiveFile(1); err != nil {
				db.releaseWriterLock()
				return nil, err
			}
			db.nextFileID = 2
		}
		return db, nil
	}

	db.activeFileID = ids[len(ids)-1]
	db.nextFileID = db.activeFileID + 1

	for _, id := range ids {
		if id == db.activeFileID {
			if err := db.scanDataFile(id, opts.ReadWrite); err != nil {
				db.releaseWriterLock()
				return nil, err
			}
			continue
		}

		if err := db.scanHintFile(id); err != nil {
			if err := db.scanDataFile(id, false); err != nil {
				db.releaseWriterLock()
				return nil, err
			}
		}
	}

	if opts.ReadWrite {
		activePath := dataFilePath(dir, db.activeFileID)
		f, err := os.OpenFile(activePath, os.O_RDWR|os.O_APPEND, 0o644)
		if err != nil {
			db.releaseWriterLock()
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			db.releaseWriterLock()
			return nil, err
		}
		db.activeFile = f
		db.activeOffset = st.Size()
	}

	return db, nil
}

// acquireWriterLock enforces the single read-write opener semantics from Bitcask.
func (db *DB) acquireWriterLock() error {
	lockPath := filepath.Join(db.dir, writerLockFileName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrWriterLocked
		}
		return err
	}
	db.lockFile = f
	return nil
}

// releaseWriterLock pairs with acquireWriterLock and is called during close/error unwind.
func (db *DB) releaseWriterLock() {
	if db.lockFile == nil {
		return
	}
	_ = syscall.Flock(int(db.lockFile.Fd()), syscall.LOCK_UN)
	_ = db.lockFile.Close()
	db.lockFile = nil
}

func (db *DB) scanHintFile(fileID uint64) error {
	path := hintFilePath(db.dir, fileID)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	for {
		hr, _, err := decodeHintRecord(f)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		entry := KeyDirEntry{
			FileID:     fileID,
			FileOffset: hr.recordOff,
			RecordSize: hr.recordSize,
			TimeStamp:  hr.timestamp,
		}
		db.applyKeydirUpdate(hr.key, entry, hr.tombstone)
	}
}

func (db *DB) scanDataFile(fileID uint64, allowTailTruncate bool) error {
	path := dataFilePath(db.dir, fileID)
	flags := os.O_RDONLY
	if allowTailTruncate {
		flags = os.O_RDWR
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	offset := int64(0)
	lastGoodOffset := int64(0)
	for {
		rec, size, err := decodeDataRecord(f)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if allowTailTruncate && isTailTruncationError(err) {
				// If the writer crashed mid-record, trim the active file to the last complete entry.
				if truncErr := f.Truncate(lastGoodOffset); truncErr != nil {
					return truncErr
				}
				if _, seekErr := f.Seek(lastGoodOffset, io.SeekStart); seekErr != nil {
					return seekErr
				}
				if syncErr := f.Sync(); syncErr != nil {
					return syncErr
				}
				return nil
			}
			if errors.Is(err, ErrCorruptData) {
				return ErrCorruptData
			}
			if !allowTailTruncate {
				return ErrCorruptData
			}
			return err
		}

		entry := KeyDirEntry{
			FileID:     fileID,
			FileOffset: offset,
			RecordSize: uint32(size),
			TimeStamp:  rec.timestamp,
		}
		tombstone := bytes.Equal(rec.value, []byte(tombstoneValue))
		db.applyKeydirUpdate(rec.key, entry, tombstone)
		offset += int64(size)
		lastGoodOffset = offset
	}
}

func isTailTruncationError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// applyKeydirUpdate resolves duplicates with Bitcask ordering: newer timestamp wins,
// then higher file ID, then higher offset for deterministic tie-breaking.
func (db *DB) applyKeydirUpdate(key string, incoming KeyDirEntry, tombstone bool) {
	current, exists := db.keydir[key]
	if exists && !shouldReplace(current, incoming) {
		return
	}
	if tombstone {
		delete(db.keydir, key)
		return
	}
	db.keydir[key] = incoming
}

func shouldReplace(current KeyDirEntry, incoming KeyDirEntry) bool {
	if incoming.TimeStamp > current.TimeStamp {
		return true
	}
	if incoming.TimeStamp < current.TimeStamp {
		return false
	}
	if incoming.FileID > current.FileID {
		return true
	}
	if incoming.FileID < current.FileID {
		return false
	}
	return incoming.FileOffset > current.FileOffset
}

func (db *DB) mustReadWrite() error {
	if !db.opts.ReadWrite {
		return ErrReadOnly
	}
	return nil
}

func (db *DB) ensureOpen() error {
	if db.closed {
		return os.ErrClosed
	}
	return nil
}

func reservedValueError() error {
	return fmt.Errorf("value %q is reserved for tombstones", tombstoneValue)
}
