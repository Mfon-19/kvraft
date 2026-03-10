package server

import (
	"context"
	"errors"
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
		completed:     make(map[pendingKey]error),
		closedCh:      make(chan struct{}),
		commitTimeout: 100 * time.Millisecond,
	}
}

type mockRaftBackend struct {
	submitFn        func(cmd raft.Command) (int, int, bool)
	isLeaderFn      func() bool
	leaderIDFn      func() int
	requestVoteFn   func(args *raft.RequestVoteArgs, reply *raft.RequestVoteReply)
	appendEntriesFn func(args *raft.AppendEntriesArgs, reply *raft.AppendEntriesReply)
	shutdownFn      func()
}

func (m *mockRaftBackend) HandleRequestVote(args *raft.RequestVoteArgs, reply *raft.RequestVoteReply) {
	if m.requestVoteFn != nil {
		m.requestVoteFn(args, reply)
	}
}

func (m *mockRaftBackend) HandleAppendEntries(args *raft.AppendEntriesArgs, reply *raft.AppendEntriesReply) {
	if m.appendEntriesFn != nil {
		m.appendEntriesFn(args, reply)
	}
}

func (m *mockRaftBackend) Submit(cmd raft.Command) (int, int, bool) {
	if m.submitFn != nil {
		return m.submitFn(cmd)
	}
	return -1, -1, false
}

func (m *mockRaftBackend) IsLeader() bool {
	if m.isLeaderFn != nil {
		return m.isLeaderFn()
	}
	return false
}

func (m *mockRaftBackend) LeaderID() int {
	if m.leaderIDFn != nil {
		return m.leaderIDFn()
	}
	return -1
}

func (m *mockRaftBackend) Shutdown() {
	if m.shutdownFn != nil {
		m.shutdownFn()
	}
}

func TestGRPCKVGetNotLeader(t *testing.T) {
	s := newBareServer(t)
	s.clientPeers = map[int]string{1: "localhost:8001"}
	s.raftNode = &mockRaftBackend{
		leaderIDFn: func() int { return 1 },
	}
	api := &GRPCKVService{server: s}

	resp, err := api.Get(context.Background(), &pb.KVRequest{Key: "missing"})
	if err != nil {
		t.Fatalf("grpc get returned rpc err: %v", err)
	}
	if resp.Success || resp.Error != ErrNotLeader.Error() {
		t.Fatalf("expected not-leader get response, got %+v", resp)
	}
	if resp.Leader != "localhost:8001" {
		t.Fatalf("expected leader hint localhost:8001, got %+v", resp)
	}
}

func TestGRPCKVGetLinearizableLeader(t *testing.T) {
	s := newBareServer(t)
	if err := s.store.Put("foo", "bar"); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	applyCh := make(chan raft.ApplyMsg, 1)
	done := make(chan struct{})
	go func() {
		s.applyCommittedEntries(applyCh)
		close(done)
	}()
	t.Cleanup(func() {
		close(s.closedCh)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("apply loop did not stop after close")
		}
	})

	s.raftNode = &mockRaftBackend{
		submitFn: func(cmd raft.Command) (int, int, bool) {
			if cmd.Type != commandTypeReadBarrier {
				t.Fatalf("expected read barrier command, got %+v", cmd)
			}
			go func() {
				applyCh <- raft.ApplyMsg{
					Index:   1,
					Term:    7,
					Command: cmd,
				}
			}()
			return 1, 7, true
		},
		isLeaderFn: func() bool { return true },
	}

	api := &GRPCKVService{server: s}
	found, err := api.Get(context.Background(), &pb.KVRequest{Key: "foo"})
	if err != nil {
		t.Fatalf("grpc get returned rpc err: %v", err)
	}
	if !found.Success || found.Value != "bar" {
		t.Fatalf("expected success with bar, got %+v", found)
	}
}

func TestPutDeleteReturnNotLeader(t *testing.T) {
	s := newBareServer(t)

	if err := s.Put("k", "v"); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader on put, got %v", err)
	}
	if err := s.Delete("k"); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader on delete, got %v", err)
	}
}

func TestGRPCKVPutDeleteNotLeader(t *testing.T) {
	s := newBareServer(t)
	s.clientPeers = map[int]string{2: "localhost:8002"}
	s.raftNode = &mockRaftBackend{
		leaderIDFn: func() int { return 2 },
	}
	api := &GRPCKVService{server: s}

	putResp, err := api.Put(context.Background(), &pb.KVRequest{Key: "k", Value: "v"})
	if err != nil {
		t.Fatalf("grpc put returned rpc err: %v", err)
	}
	if putResp.Success || putResp.Error != ErrNotLeader.Error() {
		t.Fatalf("expected not-leader put response, got %+v", putResp)
	}
	if putResp.Leader != "localhost:8002" {
		t.Fatalf("expected leader hint localhost:8002 on put, got %+v", putResp)
	}

	delResp, err := api.Delete(context.Background(), &pb.KVRequest{Key: "k"})
	if err != nil {
		t.Fatalf("grpc delete returned rpc err: %v", err)
	}
	if delResp.Success || delResp.Error != ErrNotLeader.Error() {
		t.Fatalf("expected not-leader delete response, got %+v", delResp)
	}
	if delResp.Leader != "localhost:8002" {
		t.Fatalf("expected leader hint localhost:8002 on delete, got %+v", delResp)
	}
}

func TestNormalizeConfigDerivesClientPeerAddresses(t *testing.T) {
	cfg, err := normalizeConfig(Config{
		ID:            1,
		RaftAddress:   "localhost:6001",
		ClientAddress: "localhost:8001",
		Peers:         []string{"localhost:6000", "localhost:6001", "localhost:6002"},
	})
	if err != nil {
		t.Fatalf("normalizeConfig returned err: %v", err)
	}

	expected := []string{"localhost:8000", "localhost:8001", "localhost:8002"}
	for i, addr := range expected {
		if cfg.ClientPeers[i] != addr {
			t.Fatalf("expected client peer %d to be %q, got %q", i, addr, cfg.ClientPeers[i])
		}
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

	got, err := s.store.Get("k")
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

	_, err = s.store.Get("k")
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

func TestSubmitAndWaitUsesCompletedApplyNotification(t *testing.T) {
	s := newBareServer(t)
	waitKey := pendingKey{index: 3, term: 9}
	s.completed[waitKey] = nil
	s.raftNode = &mockRaftBackend{
		submitFn: func(cmd raft.Command) (int, int, bool) {
			if cmd.Type != commandTypeReadBarrier {
				t.Fatalf("expected read barrier command, got %+v", cmd)
			}
			return waitKey.index, waitKey.term, true
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.submitAndWait(ctx, raft.Command{Type: commandTypeReadBarrier}); err != nil {
		t.Fatalf("submitAndWait returned err: %v", err)
	}
	if _, ok := s.completed[waitKey]; ok {
		t.Fatalf("expected completed notification to be consumed")
	}
}
