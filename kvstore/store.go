package kvstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMaxDataFileSize int64 = 64 * 1024 * 1024
	dataFileExtension            = ".data"
	hintFileExtension            = ".hint"
	tmpFileSuffix                = ".tmp"
	writerLockFileName           = "bitcask.write.lock"
	tombstoneValue               = "__bitcask_tombstone__"
)

var (
	ErrKeyNotFound  = errors.New("key not found")
	ErrReadOnly     = errors.New("database is read-only")
	ErrWriterLocked = errors.New("database is already open by another writer")
	ErrCorruptData  = errors.New("corrupt bitcask data")
)

type OpenOptions struct {
	ReadWrite            bool
	SyncOnPut            bool
	MaxDataFileSizeBytes int64
}

type DB struct {
	mu sync.RWMutex

	dir  string
	opts OpenOptions

	keydir map[string]KeyDirEntry

	activeFile   *os.File
	activeFileID uint64
	activeOffset int64
	nextFileID   uint64

	lockFile *os.File
	closed   bool
}

type KeyDirEntry struct {
	FileID     uint64
	FileOffset int64
	RecordSize uint32
	TimeStamp  int64
}

type decodedRecord struct {
	timestamp int64
	key       string
	value     []byte
}

type hintRecord struct {
	timestamp  int64
	key        string
	recordOff  int64
	recordSize uint32
	tombstone  bool
}

type mergeFile struct {
	fileID      uint64
	tmpDataPath string
	tmpHintPath string
	dataPath    string
	hintPath    string
	dataFile    *os.File
	hintFile    *os.File
	offset      int64
}

func Open(dir string) (*DB, error) {
	return OpenWithOptions(dir, OpenOptions{})
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
		dir:    dir,
		opts:   opts,
		keydir: make(map[string]KeyDirEntry),
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
			f.Close()
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
		f.Close()
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

func (db *DB) validateLayout() error {
	entries, err := os.ReadDir(db.dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == writerLockFileName {
			continue
		}
		if strings.HasSuffix(name, tmpFileSuffix) {
			if db.opts.ReadWrite {
				_ = os.Remove(filepath.Join(db.dir, name))
				continue
			}
			return fmt.Errorf("temporary file exists in read-only mode: %s", name)
		}
		if strings.HasSuffix(name, dataFileExtension) {
			if _, ok := parseFileID(name, dataFileExtension); ok {
				continue
			}
			return fmt.Errorf("invalid data filename: %s", name)
		}
		if strings.HasSuffix(name, hintFileExtension) {
			if _, ok := parseFileID(name, hintFileExtension); ok {
				continue
			}
			return fmt.Errorf("invalid hint filename: %s", name)
		}
		return fmt.Errorf("unsupported legacy file detected: %s", name)
	}

	return nil
}

func (db *DB) scanHintFile(fileID uint64) error {
	path := hintFilePath(db.dir, fileID)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	offset := int64(0)
	for {
		hr, size, err := decodeHintRecord(f)
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
		offset += int64(size)
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
			if allowTailTruncate {
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
			return ErrCorruptData
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

	path := dataFilePath(db.dir, entry.FileID)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(entry.FileOffset, io.SeekStart); err != nil {
		return nil, err
	}

	rec, size, err := decodeDataRecord(f)
	if err != nil {
		if errors.Is(err, ErrCorruptData) {
			return nil, ErrCorruptData
		}
		return nil, err
	}
	if rec.key != key {
		return nil, ErrCorruptData
	}
	if uint32(size) != entry.RecordSize {
		return nil, ErrCorruptData
	}
	if bytes.Equal(rec.value, []byte(tombstoneValue)) {
		return nil, ErrKeyNotFound
	}
	return rec.value, nil
}

func (db *DB) Put(key string, value string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return os.ErrClosed
	}
	if !db.opts.ReadWrite {
		return ErrReadOnly
	}
	if value == tombstoneValue {
		return fmt.Errorf("value %q is reserved for tombstones", tombstoneValue)
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

	if db.closed {
		return os.ErrClosed
	}
	if !db.opts.ReadWrite {
		return ErrReadOnly
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
		f.Close()
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
		hintFile.Close()
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

func (db *DB) Merge() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return os.ErrClosed
	}
	if !db.opts.ReadWrite {
		return ErrReadOnly
	}

	allIDs, err := listDataFileIDs(db.dir)
	if err != nil {
		return err
	}
	if len(allIDs) == 0 {
		return nil
	}

	immutableIDs := make([]uint64, 0, len(allIDs))
	immutableSet := make(map[uint64]struct{})
	for _, id := range allIDs {
		if id == db.activeFileID {
			continue
		}
		immutableIDs = append(immutableIDs, id)
		immutableSet[id] = struct{}{}
	}
	if len(immutableIDs) == 0 {
		return nil
	}

	type mergeCandidate struct {
		key   string
		entry KeyDirEntry
	}
	candidates := make([]mergeCandidate, 0)
	for k, e := range db.keydir {
		if _, ok := immutableSet[e.FileID]; ok {
			candidates = append(candidates, mergeCandidate{key: k, entry: e})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].key < candidates[j].key
	})

	mergedFiles := make([]*mergeFile, 0)
	mergedEntries := make(map[string]KeyDirEntry, len(candidates))
	var current *mergeFile

	for _, candidate := range candidates {
		rec, err := db.readRecordAtEntry(candidate.key, candidate.entry)
		if err != nil {
			return err
		}
		recBytes, err := encodeDataRecord(rec.timestamp, candidate.key, rec.value)
		if err != nil {
			return err
		}

		recordLen := int64(len(recBytes))
		if current == nil || (current.offset+recordLen > db.opts.MaxDataFileSizeBytes && current.offset > 0) {
			if current != nil {
				if err := current.closeAndSync(); err != nil {
					return err
				}
			}
			current, err = db.newMergeFile()
			if err != nil {
				return err
			}
			mergedFiles = append(mergedFiles, current)
		}

		offset := current.offset
		if _, err := current.dataFile.Write(recBytes); err != nil {
			return err
		}
		entry := KeyDirEntry{
			FileID:     current.fileID,
			FileOffset: offset,
			RecordSize: uint32(recordLen),
			TimeStamp:  rec.timestamp,
		}
		h := hintRecord{
			timestamp:  rec.timestamp,
			key:        candidate.key,
			recordOff:  offset,
			recordSize: uint32(recordLen),
			tombstone:  false,
		}
		if err := encodeHintRecord(current.hintFile, h); err != nil {
			return err
		}
		current.offset += recordLen
		mergedEntries[candidate.key] = entry
	}

	if current != nil {
		if err := current.closeAndSync(); err != nil {
			return err
		}
	}

	// Publish merged segments in order: write files, fsync files, atomically rename,
	// then remove old immutable segments after keydir points at merged files.
	for _, mf := range mergedFiles {
		if err := os.Rename(mf.tmpDataPath, mf.dataPath); err != nil {
			return err
		}
		if err := os.Rename(mf.tmpHintPath, mf.hintPath); err != nil {
			return err
		}
	}

	for key, entry := range mergedEntries {
		db.keydir[key] = entry
	}

	for _, oldID := range immutableIDs {
		_ = os.Remove(dataFilePath(db.dir, oldID))
		_ = os.Remove(hintFilePath(db.dir, oldID))
	}

	if err := db.rotateActiveFileLocked(); err != nil {
		return err
	}

	return nil
}

func (db *DB) newMergeFile() (*mergeFile, error) {
	fileID := db.nextFileID
	db.nextFileID++

	dataPath := dataFilePath(db.dir, fileID)
	hintPath := hintFilePath(db.dir, fileID)
	tmpData := dataPath + tmpFileSuffix
	tmpHint := hintPath + tmpFileSuffix

	df, err := os.OpenFile(tmpData, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	hf, err := os.OpenFile(tmpHint, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		df.Close()
		_ = os.Remove(tmpData)
		return nil, err
	}

	return &mergeFile{
		fileID:      fileID,
		tmpDataPath: tmpData,
		tmpHintPath: tmpHint,
		dataPath:    dataPath,
		hintPath:    hintPath,
		dataFile:    df,
		hintFile:    hf,
		offset:      0,
	}, nil
}

func (mf *mergeFile) closeAndSync() error {
	if err := mf.dataFile.Sync(); err != nil {
		_ = mf.dataFile.Close()
		_ = mf.hintFile.Close()
		return err
	}
	if err := mf.hintFile.Sync(); err != nil {
		_ = mf.dataFile.Close()
		_ = mf.hintFile.Close()
		return err
	}
	if err := mf.dataFile.Close(); err != nil {
		_ = mf.hintFile.Close()
		return err
	}
	if err := mf.hintFile.Close(); err != nil {
		return err
	}
	return nil
}

func (db *DB) readRecordAtEntry(key string, entry KeyDirEntry) (*decodedRecord, error) {
	path := dataFilePath(db.dir, entry.FileID)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(entry.FileOffset, io.SeekStart); err != nil {
		return nil, err
	}
	rec, size, err := decodeDataRecord(f)
	if err != nil {
		return nil, err
	}
	if rec.key != key || uint32(size) != entry.RecordSize {
		return nil, ErrCorruptData
	}
	if bytes.Equal(rec.value, []byte(tombstoneValue)) {
		return nil, ErrCorruptData
	}
	return &rec, nil
}

func (db *DB) Sync() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return os.ErrClosed
	}
	if !db.opts.ReadWrite {
		return ErrReadOnly
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

	db.releaseWriterLock()
	db.closed = true
	return closeErr
}

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

func listDataFileIDs(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := parseFileID(e.Name(), dataFileExtension)
		if ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func parseFileID(name string, suffix string) (uint64, bool) {
	if !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	base := strings.TrimSuffix(name, suffix)
	if base == "" {
		return 0, false
	}
	id, err := strconv.ParseUint(base, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func dataFilePath(dir string, fileID uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%020d%s", fileID, dataFileExtension))
}

func hintFilePath(dir string, fileID uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%020d%s", fileID, hintFileExtension))
}
