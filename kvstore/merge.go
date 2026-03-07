package kvstore

import "os"

func (db *DB) Merge() error {
	db.mu.Lock()
	if err := db.ensureOpen(); err != nil {
		db.mu.Unlock()
		return err
	}
	if err := db.mustReadWrite(); err != nil {
		db.mu.Unlock()
		return err
	}

	allIDs, err := listDataFileIDs(db.dir)
	if err != nil {
		db.mu.Unlock()
		return err
	}
	if len(allIDs) == 0 {
		db.mu.Unlock()
		return nil
	}

	immutableSet := make(map[uint64]struct{})
	immutableIDs := make([]uint64, 0, len(allIDs))
	for _, id := range allIDs {
		if id == db.activeFileID {
			continue
		}
		immutableSet[id] = struct{}{}
		immutableIDs = append(immutableIDs, id)
	}
	if len(immutableIDs) == 0 {
		db.mu.Unlock()
		return nil
	}

	snapshot := make(map[string]KeyDirEntry)
	for key, entry := range db.keydir {
		if _, ok := immutableSet[entry.FileID]; ok {
			snapshot[key] = entry
		}
	}
	db.mu.Unlock()

	keys := sortedKeys(snapshot)
	mergedEntries := make(map[string]KeyDirEntry, len(snapshot))
	mergedFiles := make([]*mergeFile, 0)
	var current *mergeFile
	mergePublished := false
	defer func() {
		if mergePublished {
			return
		}
		cleanupMergeFiles(mergedFiles)
		cleanupMergeArtifacts(current)
	}()

	for _, key := range keys {
		entry := snapshot[key]
		rec, err := db.readRecordAtEntry(key, entry)
		if err != nil {
			return err
		}

		recBytes, err := encodeDataRecord(rec.timestamp, key, rec.value)
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
		h := hintRecord{
			timestamp:  rec.timestamp,
			key:        key,
			recordOff:  offset,
			recordSize: uint32(recordLen),
			tombstone:  false,
		}
		if err := encodeHintRecord(current.hintFile, h); err != nil {
			return err
		}
		current.offset += recordLen

		mergedEntries[key] = KeyDirEntry{
			FileID:     current.fileID,
			FileOffset: offset,
			RecordSize: uint32(recordLen),
			TimeStamp:  rec.timestamp,
		}
	}

	if current != nil {
		if err := current.closeAndSync(); err != nil {
			return err
		}
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if err := db.ensureOpen(); err != nil {
		return err
	}
	if err := db.mustReadWrite(); err != nil {
		return err
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

	for key, newEntry := range mergedEntries {
		currentEntry, ok := db.keydir[key]
		if !ok {
			continue
		}
		if !sameEntry(currentEntry, snapshot[key]) {
			continue
		}
		db.keydir[key] = newEntry
	}

	db.readersMu.Lock()
	defer db.readersMu.Unlock()

	for _, oldID := range immutableIDs {
		db.closeReaderLocked(oldID)
		_ = os.Remove(dataFilePath(db.dir, oldID))
		_ = os.Remove(hintFilePath(db.dir, oldID))
	}

	mergePublished = true
	return nil
}

func sameEntry(a, b KeyDirEntry) bool {
	return a.FileID == b.FileID &&
		a.FileOffset == b.FileOffset &&
		a.RecordSize == b.RecordSize &&
		a.TimeStamp == b.TimeStamp
}

func (db *DB) newMergeFile() (*mergeFile, error) {
	fileID, err := db.allocateDataFileID()
	if err != nil {
		return nil, err
	}

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
		_ = df.Close()
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

func cleanupMergeArtifacts(mf *mergeFile) {
	if mf == nil {
		return
	}
	if mf.dataFile != nil {
		_ = mf.dataFile.Close()
	}
	if mf.hintFile != nil {
		_ = mf.hintFile.Close()
	}
	_ = os.Remove(mf.tmpDataPath)
	_ = os.Remove(mf.tmpHintPath)
}

func cleanupMergeFiles(files []*mergeFile) {
	for _, mf := range files {
		cleanupMergeArtifacts(mf)
	}
}
