package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"kvraft/kvstore"
	"kvraft/raft"
)

func newTestStore(t *testing.T) *kvstore.DB {
	t.Helper()
	db, err := kvstore.OpenWithOptions(t.TempDir(), kvstore.OpenOptions{
		ReadWrite:            true,
		SyncOnPut:            false,
		MaxDataFileSizeBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db
}

func TestHandleClientRequestUnknownCommand(t *testing.T) {
	s := &RaftKVServer{
		store:   newTestStore(t),
		pending: make(map[int]chan string),
	}

	resp := s.HandleClientRequest(ClientRequest{Type: "unknown"})
	if resp.Success {
		t.Fatalf("expected unknown command to fail")
	}
	if resp.Error != "unknown command" {
		t.Fatalf("unexpected error %q", resp.Error)
	}
}

func TestHandleClientRequestGetPaths(t *testing.T) {
	s := &RaftKVServer{
		store:   newTestStore(t),
		pending: make(map[int]chan string),
	}

	resp := s.HandleClientRequest(ClientRequest{Type: "get", Key: "missing"})
	if resp.Success {
		t.Fatalf("expected get missing to fail")
	}
	if !strings.Contains(resp.Error, kvstore.ErrKeyNotFound.Error()) {
		t.Fatalf("expected key-not-found error, got %q", resp.Error)
	}

	if err := s.store.Put("foo", "bar"); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	resp = s.HandleClientRequest(ClientRequest{Type: "get", Key: "foo"})
	if !resp.Success {
		t.Fatalf("expected get success, got error %q", resp.Error)
	}
	if resp.Value != "bar" {
		t.Fatalf("expected value bar got %q", resp.Value)
	}
}

func TestPutDeleteReturnNotLeader(t *testing.T) {
	s := &RaftKVServer{
		raftNode: &raft.Node{},
		store:    newTestStore(t),
		pending:  make(map[int]chan string),
	}

	if err := s.Put("k", "v"); err == nil || err.Error() != "not leader" {
		t.Fatalf("expected not leader on put, got %v", err)
	}
	if err := s.Delete("k"); err == nil || err.Error() != "not leader" {
		t.Fatalf("expected not leader on delete, got %v", err)
	}
}

func TestHandleClientRequestPutDeleteNotLeader(t *testing.T) {
	s := &RaftKVServer{
		raftNode: &raft.Node{},
		store:    newTestStore(t),
		pending:  make(map[int]chan string),
	}

	putResp := s.HandleClientRequest(ClientRequest{Type: "put", Key: "k", Value: "v"})
	if putResp.Success || putResp.Error != "not leader" {
		t.Fatalf("expected put not leader error, got %+v", putResp)
	}

	deleteResp := s.HandleClientRequest(ClientRequest{Type: "delete", Key: "k"})
	if deleteResp.Success || deleteResp.Error != "not leader" {
		t.Fatalf("expected delete not leader error, got %+v", deleteResp)
	}
}

func TestApplyCommittedEntriesUpdatesStoreAndSignalsPending(t *testing.T) {
	s := &RaftKVServer{
		store:   newTestStore(t),
		pending: make(map[int]chan string),
	}

	applyCh := make(chan raft.ApplyMsg, 4)
	done := make(chan struct{})
	go func() {
		s.applyCommittedEntries(applyCh)
		close(done)
	}()

	putSignal := make(chan string, 1)
	s.pendingLock.Lock()
	s.pending[1] = putSignal
	s.pendingLock.Unlock()

	applyCh <- raft.ApplyMsg{
		Index: 1,
		Command: raft.Command{
			Type:  "put",
			Key:   "k",
			Value: "v",
		},
	}

	select {
	case <-putSignal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for put apply notification")
	}

	got, err := s.Get("k")
	if err != nil {
		t.Fatalf("get after put apply: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("expected value v got %q", string(got))
	}

	deleteSignal := make(chan string, 1)
	s.pendingLock.Lock()
	s.pending[2] = deleteSignal
	s.pendingLock.Unlock()

	applyCh <- raft.ApplyMsg{
		Index: 2,
		Command: raft.Command{
			Type: "delete",
			Key:  "k",
		},
	}

	select {
	case <-deleteSignal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for delete apply notification")
	}

	_, err = s.Get("k")
	if !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("expected key not found after delete apply, got %v", err)
	}

	close(applyCh)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("apply loop did not exit after channel close")
	}
}
