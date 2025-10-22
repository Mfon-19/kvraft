package kvraft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type DB struct {
	path       string
	activeFile string
	keyDir     map[string]KeyDirEntry
	mu         *sync.RWMutex
}

type KeyDirEntry struct {
	filePath  string
	entrySize uint32
	filePos   uint32
	timeStamp time.Time
}

type Entry struct {
	crc       crc32.Table
	timeStamp time.Time
	keySize   uint32
	valueSize uint32
	key       string
	value     []byte
}

func Open(dirName string) (*DB, error) {
	// the directory for all the files to lie in
	err := os.MkdirAll(dirName, 0755)
	if err != nil {
		return nil, errors.New("an error occurred while creating directory")
	}

	// use name 'file1' for the first file because the name is irrelevant (I think)
	filePath := filepath.Join(dirName, "file1")

	return &DB{
		path:       dirName,
		activeFile: filePath,
		keyDir:     make(map[string]KeyDirEntry),
		mu:         &sync.RWMutex{},
	}, nil
}

func (db *DB) Get(key string) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	// check if the key exists
	entry, exists := db.keyDir[key]
	if !exists {
		return nil, errors.New("key does not exist")
	}

	file, err := os.Open(entry.filePath)
	if err != nil {
		return nil, errors.New("failed to open file for reading")
	}
	defer file.Close()

	// read len(crc) + entrySize bytes from the offset specified in the keyDir map
	_, err = file.Seek(int64(entry.filePos), io.SeekStart)
	if err != nil {
		return nil, err
	}

	// Read CRC
	var storedCRC uint32
	err = binary.Read(file, binary.LittleEndian, &storedCRC)
	if err != nil {
		return nil, err
	}

	// Read the rest into buffer
	var timestamp int64
	var keySize, valueSize uint32

	err = binary.Read(file, binary.LittleEndian, &timestamp)
	if err != nil {
		return nil, err
	}
	err = binary.Read(file, binary.LittleEndian, &keySize)
	if err != nil {
		return nil, err
	}
	err = binary.Read(file, binary.LittleEndian, &valueSize)
	if err != nil {
		return nil, err
	}

	k := make([]byte, keySize)
	value := make([]byte, valueSize)
	_, err = file.Read(k)
	if err != nil {
		return nil, err
	}
	_, err = file.Read(value)
	if err != nil {
		return nil, err
	}

	// Verify CRC
	var checkBuf bytes.Buffer
	err = binary.Write(&checkBuf, binary.LittleEndian, timestamp)
	if err != nil {
		return nil, err
	}
	err = binary.Write(&checkBuf, binary.LittleEndian, keySize)
	if err != nil {
		return nil, err
	}
	err = binary.Write(&checkBuf, binary.LittleEndian, valueSize)
	if err != nil {
		return nil, err
	}
	checkBuf.Write(k)
	checkBuf.Write(value)

	calculatedCRC := crc32.ChecksumIEEE(checkBuf.Bytes())
	if calculatedCRC != storedCRC {
		return nil, errors.New("crc values do not match. the data is corrupted")
	}

	db.mu.RLock()
	return value, nil
}

func (db *DB) Put(key string, value string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	file, err := os.OpenFile(db.activeFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return errors.New("an error occurred while opening file for append")
	}
	defer file.Close()

	info, _ := file.Stat()
	offSet := info.Size()

	// for now, I'm going to ignore file sizes and use only one active file for appends
	timeStamp := time.Now()
	keySize := uint32(len(key))
	valueSize := uint32(len(value))
	var buf bytes.Buffer

	err = binary.Write(&buf, binary.LittleEndian, timeStamp.UnixNano())
	if err != nil {
		return errors.New("an error occurred")
	}
	err = binary.Write(&buf, binary.LittleEndian, keySize)
	if err != nil {
		return errors.New("an error occurred")
	}
	err = binary.Write(&buf, binary.LittleEndian, valueSize)
	if err != nil {
		return errors.New("an error occurred")
	}
	buf.Write([]byte(key))
	buf.Write([]byte(value))

	data := buf.Bytes()
	crc := crc32.ChecksumIEEE(data)

	err = binary.Write(file, binary.LittleEndian, crc)
	if err != nil {
		return errors.New("an error occurred while appending crc to file")
	}
	_, err = file.Write(data)
	if err != nil {
		return errors.New("an error occurred while appending data to file")
	}

	db.keyDir[key] = KeyDirEntry{
		filePath:  db.activeFile,
		entrySize: uint32(crc32.Size + len(data)),
		filePos:   uint32(offSet),
		timeStamp: timeStamp,
	}

	return nil
}
