package kvstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func openWritable(t *testing.T, dir string, maxSize int64, syncOnPut bool) *DB {
	t.Helper()
	db, err := OpenWithOptions(dir, OpenOptions{
		ReadWrite:            true,
		SyncOnPut:            syncOnPut,
		MaxDataFileSizeBytes: maxSize,
	})
	if err != nil {
		t.Fatalf("open writable: %v", err)
	}
	return db
}

func TestPutGetOverwriteAndReopen(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, defaultMaxDataFileSize, false)

	if err := db.Put("k", "v1"); err != nil {
		t.Fatalf("put 1: %v", err)
	}
	if err := db.Put("k", "v2"); err != nil {
		t.Fatalf("put 2: %v", err)
	}
	v, err := db.Get("k")
	if err != nil {
		t.Fatalf("get before close: %v", err)
	}
	if string(v) != "v2" {
		t.Fatalf("expected v2 got %q", string(v))
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("reopen read-only: %v", err)
	}
	defer ro.Close()

	v, err = ro.Get("k")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if string(v) != "v2" {
		t.Fatalf("expected v2 after reopen got %q", string(v))
	}
}

func TestOpenDefaultsToReadOnly(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open default: %v", err)
	}
	defer db.Close()

	if err := db.Put("k", "v"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected ErrReadOnly from default open put, got %v", err)
	}
}

func TestCRCDetectionOnGet(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, defaultMaxDataFileSize, false)
	if err := db.Put("k", "v"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()

	ids, err := listDataFileIDs(dir)
	if err != nil || len(ids) == 0 {
		t.Fatalf("list ids err=%v len=%d", err, len(ids))
	}
	path := dataFilePath(dir, ids[0])
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open data file: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		t.Fatalf("stat: %v", err)
	}
	if st.Size() < 1 {
		f.Close()
		t.Fatal("unexpected empty data file")
	}
	if _, err := f.WriteAt([]byte{0xAA}, st.Size()-1); err != nil {
		f.Close()
		t.Fatalf("corrupt write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	if _, err := ro.Get("k"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("expected ErrCorruptData got %v", err)
	}
}

func TestDeletePersistsWithTombstone(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, defaultMaxDataFileSize, false)

	if err := db.Put("k", "v"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := db.Delete("k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ro.Close()

	if _, err := ro.Get("k"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound got %v", err)
	}
}

func TestRotationAndReadsAcrossImmutableFiles(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, 120, false)
	defer db.Close()

	for i := 0; i < 12; i++ {
		if err := db.Put(fmt.Sprintf("k%02d", i), strings.Repeat("v", 8)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	ids, err := listDataFileIDs(dir)
	if err != nil {
		t.Fatalf("list ids: %v", err)
	}
	if len(ids) < 2 {
		t.Fatalf("expected rotation to create multiple .data files, got %d", len(ids))
	}

	v, err := db.Get("k00")
	if err != nil {
		t.Fatalf("get early key: %v", err)
	}
	if string(v) != strings.Repeat("v", 8) {
		t.Fatalf("unexpected value %q", string(v))
	}
}

func TestHintRebuildFallbackToDataScan(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, 130, false)

	for i := 0; i < 16; i++ {
		if err := db.Put(fmt.Sprintf("k%02d", i), strings.Repeat("z", 8)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ids, err := listDataFileIDs(dir)
	if err != nil {
		t.Fatalf("list ids: %v", err)
	}
	if len(ids) < 3 {
		t.Fatalf("need at least 3 data files for mixed hint/data startup path, got %d", len(ids))
	}

	// Remove one immutable hint file to force fallback scanning from data.
	missingHint := hintFilePath(dir, ids[0])
	if err := os.Remove(missingHint); err != nil {
		t.Fatalf("remove hint: %v", err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open read-only with mixed hint availability: %v", err)
	}
	defer ro.Close()

	for i := 0; i < 16; i++ {
		v, err := ro.Get(fmt.Sprintf("k%02d", i))
		if err != nil {
			t.Fatalf("get %d after mixed rebuild: %v", i, err)
		}
		if string(v) != strings.Repeat("z", 8) {
			t.Fatalf("unexpected value for k%02d: %q", i, string(v))
		}
	}
}

func TestActiveTailTruncationRecovery(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, defaultMaxDataFileSize, false)
	if err := db.Put("k", "v"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := db.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	activePath := dataFilePath(dir, db.activeFileID)
	st, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	originalSize := st.Size()

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := os.OpenFile(activePath, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open active: %v", err)
	}
	if _, err := f.Write([]byte{0x01, 0x02, 0x03}); err != nil {
		f.Close()
		t.Fatalf("append garbage: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	reopened := openWritable(t, dir, defaultMaxDataFileSize, false)
	defer reopened.Close()

	v, err := reopened.Get("k")
	if err != nil {
		t.Fatalf("get after recovery: %v", err)
	}
	if string(v) != "v" {
		t.Fatalf("expected v got %q", string(v))
	}

	recoveredStat, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("stat recovered active: %v", err)
	}
	if recoveredStat.Size() != originalSize {
		t.Fatalf("expected truncated size %d got %d", originalSize, recoveredStat.Size())
	}
}

func TestActiveScanCRCErrorDoesNotTruncate(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, defaultMaxDataFileSize, false)
	if err := db.Put("a", "value-a"); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if err := db.Put("b", "value-b"); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if err := db.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	activePath := dataFilePath(dir, db.activeFileID)
	statBefore, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("stat active before close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := os.OpenFile(activePath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open active: %v", err)
	}
	orig := make([]byte, 1)
	if _, err := f.ReadAt(orig, 0); err != nil {
		f.Close()
		t.Fatalf("read crc byte: %v", err)
	}
	if _, err := f.WriteAt([]byte{orig[0] ^ 0xFF}, 0); err != nil {
		f.Close()
		t.Fatalf("flip crc byte: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	if _, err := OpenWithOptions(dir, OpenOptions{ReadWrite: true}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("expected ErrCorruptData on startup scan got %v", err)
	}

	statAfter, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("stat active after reopen failure: %v", err)
	}
	if statAfter.Size() != statBefore.Size() {
		t.Fatalf("expected active file size unchanged at %d, got %d", statBefore.Size(), statAfter.Size())
	}
}

func TestSingleWriterLockAndReadOnlyEnforcement(t *testing.T) {
	dir := t.TempDir()
	db1 := openWritable(t, dir, defaultMaxDataFileSize, false)
	defer db1.Close()

	if _, err := OpenWithOptions(dir, OpenOptions{ReadWrite: true}); !errors.Is(err, ErrWriterLocked) {
		t.Fatalf("expected ErrWriterLocked got %v", err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open read-only while writer active: %v", err)
	}
	defer ro.Close()

	if err := ro.Put("a", "b"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected ErrReadOnly on put got %v", err)
	}
	if err := ro.Delete("a"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected ErrReadOnly on delete got %v", err)
	}
	if err := ro.Merge(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected ErrReadOnly on merge got %v", err)
	}
	if err := ro.Sync(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected ErrReadOnly on sync got %v", err)
	}
}

func TestMergeDropsStaleAndTombstonedKeys(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, 120, false)

	if err := db.Put("a", "old"); err != nil {
		t.Fatalf("put a old: %v", err)
	}
	if err := db.Put("a", "new"); err != nil {
		t.Fatalf("put a new: %v", err)
	}
	if err := db.Put("b", "val"); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if err := db.Delete("b"); err != nil {
		t.Fatalf("delete b: %v", err)
	}

	for i := 0; i < 14; i++ {
		if err := db.Put(fmt.Sprintf("x%02d", i), strings.Repeat("n", 8)); err != nil {
			t.Fatalf("put x%02d: %v", i, err)
		}
	}

	if err := db.Merge(); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("reopen after merge: %v", err)
	}
	defer ro.Close()

	v, err := ro.Get("a")
	if err != nil {
		t.Fatalf("get a after merge: %v", err)
	}
	if string(v) != "new" {
		t.Fatalf("expected latest a value new got %q", string(v))
	}
	if _, err := ro.Get("b"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected b deleted after merge got %v", err)
	}

	hints, err := filepath.Glob(filepath.Join(dir, "*.hint"))
	if err != nil {
		t.Fatalf("glob hints: %v", err)
	}
	if len(hints) == 0 {
		t.Fatal("expected hint files after merge/rotation")
	}
}

func TestListKeysAndFoldLiveView(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, defaultMaxDataFileSize, false)
	defer db.Close()

	if err := db.Put("k1", "v1"); err != nil {
		t.Fatalf("put k1: %v", err)
	}
	if err := db.Put("k2", "v2"); err != nil {
		t.Fatalf("put k2: %v", err)
	}
	if err := db.Delete("k2"); err != nil {
		t.Fatalf("delete k2: %v", err)
	}

	keys := db.ListKeys()
	if len(keys) != 1 || keys[0] != "k1" {
		t.Fatalf("expected only k1 in keys, got %v", keys)
	}

	seen := make([]string, 0)
	if err := db.Fold(func(key string, value []byte) error {
		seen = append(seen, key+"="+string(value))
		return nil
	}); err != nil {
		t.Fatalf("fold: %v", err)
	}
	sort.Strings(seen)
	if len(seen) != 1 || seen[0] != "k1=v1" {
		t.Fatalf("unexpected fold output: %v", seen)
	}
}

func TestSyncAndCloseSemantics(t *testing.T) {
	dir := t.TempDir()
	db := openWritable(t, dir, defaultMaxDataFileSize, false)

	if err := db.Put("k", "v"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := db.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := db.Put("k2", "v2"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("expected os.ErrClosed after close got %v", err)
	}
}
