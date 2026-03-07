package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kvraft/kvstore"
	pb "kvraft/proto"
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

func newBareServer(t *testing.T) *RaftKVServer {
	t.Helper()
	return &RaftKVServer{
		store:         newTestStore(t),
		pending:       make(map[pendingKey]chan error),
		closedCh:      make(chan struct{}),
		commitTimeout: 100 * time.Millisecond,
	}
}

func TestGRPCKVGetPaths(t *testing.T) {
	s := newBareServer(t)
	api := &GRPCKVService{server: s}

	missing, err := api.Get(context.Background(), &pb.KVRequest{Key: "missing"})
	if err != nil {
		t.Fatalf("grpc get missing returned rpc err: %v", err)
	}
	if missing.Success {
		t.Fatalf("expected missing key get to fail")
	}
	if !strings.Contains(missing.Error, kvstore.ErrKeyNotFound.Error()) {
		t.Fatalf("expected key-not-found error, got %q", missing.Error)
	}

	if err := s.store.Put("foo", "bar"); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	found, err := api.Get(context.Background(), &pb.KVRequest{Key: "foo"})
	if err != nil {
		t.Fatalf("grpc get existing returned rpc err: %v", err)
	}
	if !found.Success || found.Value != "bar" {
		t.Fatalf("expected success with bar, got %+v", found)
	}
}

func TestPutDeleteReturnNotLeader(t *testing.T) {
	s := newBareServer(t)
	s.raftNode = &raft.Node{}

	if err := s.Put("k", "v"); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader on put, got %v", err)
	}
	if err := s.Delete("k"); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader on delete, got %v", err)
	}
}

func TestGRPCKVPutDeleteNotLeader(t *testing.T) {
	s := newBareServer(t)
	s.raftNode = &raft.Node{}
	api := &GRPCKVService{server: s}

	putResp, err := api.Put(context.Background(), &pb.KVRequest{Key: "k", Value: "v"})
	if err != nil {
		t.Fatalf("grpc put returned rpc err: %v", err)
	}
	if putResp.Success || putResp.Error != ErrNotLeader.Error() {
		t.Fatalf("expected not-leader put response, got %+v", putResp)
	}

	delResp, err := api.Delete(context.Background(), &pb.KVRequest{Key: "k"})
	if err != nil {
		t.Fatalf("grpc delete returned rpc err: %v", err)
	}
	if delResp.Success || delResp.Error != ErrNotLeader.Error() {
		t.Fatalf("expected not-leader delete response, got %+v", delResp)
	}
}

func TestApplyCommittedEntriesUpdatesStoreAndSignalsPending(t *testing.T) {
	s := newBareServer(t)

	applyCh := make(chan raft.ApplyMsg, 4)
	done := make(chan struct{})
	go func() {
		s.applyCommittedEntries(applyCh)
		close(done)
	}()

	putSignal := make(chan error, 1)
	s.pendingLock.Lock()
	s.pending[pendingKey{index: 1, term: 3}] = putSignal
	s.pendingLock.Unlock()

	applyCh <- raft.ApplyMsg{
		Index: 1,
		Term:  3,
		Command: raft.Command{
			Type:  "put",
			Key:   "k",
			Value: "v",
		},
	}

	select {
	case err := <-putSignal:
		if err != nil {
			t.Fatalf("unexpected put apply err: %v", err)
		}
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

	delSignal := make(chan error, 1)
	s.pendingLock.Lock()
	s.pending[pendingKey{index: 2, term: 3}] = delSignal
	s.pendingLock.Unlock()

	applyCh <- raft.ApplyMsg{
		Index: 2,
		Term:  3,
		Command: raft.Command{
			Type: "delete",
			Key:  "k",
		},
	}

	select {
	case err := <-delSignal:
		if err != nil {
			t.Fatalf("unexpected delete apply err: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for delete apply notification")
	}

	_, err = s.Get("k")
	if !errors.Is(err, kvstore.ErrKeyNotFound) {
		t.Fatalf("expected key not found after delete apply, got %v", err)
	}

	close(s.closedCh)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("apply loop did not stop after close")
	}
}
