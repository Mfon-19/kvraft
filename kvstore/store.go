package kvstore

import (
	"errors"
	"os"
	"sync"
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

	// cached read descriptors for immutable/active data files.
	readersMu sync.Mutex
	readers   map[uint64]*os.File

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
