package kvstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

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
