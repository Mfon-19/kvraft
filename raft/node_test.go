package raft

import (
	"context"
	"strconv"
	"testing"
	"time"
)

type mockRPCHandler struct {
	requestVoteFn   func(ctx context.Context, target string, args *RequestVoteArgs, reply *RequestVoteReply) error
	appendEntriesFn func(ctx context.Context, target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error
}

func (m *mockRPCHandler) RequestVote(ctx context.Context, target string, args *RequestVoteArgs, reply *RequestVoteReply) error {
	if m.requestVoteFn != nil {
		return m.requestVoteFn(ctx, target, args, reply)
	}
	reply.Term = args.Term
	reply.VoteGranted = true
	return nil
}

func (m *mockRPCHandler) AppendEntries(ctx context.Context, target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error {
	if m.appendEntriesFn != nil {
		return m.appendEntriesFn(ctx, target, args, reply)
	}
	reply.Term = args.Term
	reply.Success = true
	return nil
}

func newTestNode(id int, peerCount int) *Node {
	peers := make([]string, peerCount)
	for i := range peers {
		peers[i] = "peer-" + strconv.Itoa(i)
	}

	return &Node{
		id:             id,
		peers:          peers,
		currentTerm:    1,
		votedFor:       -1,
		log:            []LogEntry{{Term: 0, Index: 0}},
		commitIndex:    0,
		lastApplied:    0,
		state:          Follower,
		applyCh:        make(chan ApplyMsg, 16),
		commitNotifyCh: make(chan struct{}, 16),
		heartbeatCh:    make(chan bool, 16),
		voteCh:         make(chan bool, 16),
		shutdownCh:     make(chan struct{}),
		rpcHandler:     &mockRPCHandler{},
		rpcTimeout:     time.Second,
	}
}

func TestHandleRequestVoteGrantAndSingleVotePerTerm(t *testing.T) {
	n := newTestNode(0, 3)
	n.currentTerm = 2
	n.log = append(n.log, LogEntry{Term: 1, Index: 1}, LogEntry{Term: 2, Index: 2})

	var first RequestVoteReply
	n.HandleRequestVote(&RequestVoteArgs{
		Term:         2,
		CandidateId:  1,
		LastLogIndex: 2,
		LastLogTerm:  2,
	}, &first)

	if !first.VoteGranted {
		t.Fatalf("expected first vote to be granted")
	}
	if n.votedFor != 1 {
		t.Fatalf("expected votedFor=1 got %d", n.votedFor)
	}

	var second RequestVoteReply
	n.HandleRequestVote(&RequestVoteArgs{
		Term:         2,
		CandidateId:  2,
		LastLogIndex: 2,
		LastLogTerm:  2,
	}, &second)

	if second.VoteGranted {
		t.Fatalf("expected second vote in same term to be rejected")
	}
}

func TestHandleRequestVoteRejectsStaleLog(t *testing.T) {
	n := newTestNode(0, 3)
	n.currentTerm = 3
	n.log = append(n.log, LogEntry{Term: 3, Index: 1})

	var reply RequestVoteReply
	n.HandleRequestVote(&RequestVoteArgs{
		Term:         3,
		CandidateId:  1,
		LastLogIndex: 99,
		LastLogTerm:  2,
	}, &reply)

	if reply.VoteGranted {
		t.Fatalf("expected vote rejection for stale candidate log")
	}
}

func TestHandleRequestVoteHigherTermResetsState(t *testing.T) {
	n := newTestNode(0, 3)
	n.currentTerm = 3
	n.votedFor = 0
	n.state = Leader
	n.log = append(n.log, LogEntry{Term: 3, Index: 1})

	var reply RequestVoteReply
	n.HandleRequestVote(&RequestVoteArgs{
		Term:         4,
		CandidateId:  2,
		LastLogIndex: 1,
		LastLogTerm:  3,
	}, &reply)

	if !reply.VoteGranted {
		t.Fatalf("expected vote to be granted for higher term candidate")
	}
	if n.currentTerm != 4 {
		t.Fatalf("expected term=4 got %d", n.currentTerm)
	}
	if n.state != Follower {
		t.Fatalf("expected state follower got %v", n.state)
	}
	if n.votedFor != 2 {
		t.Fatalf("expected votedFor=2 got %d", n.votedFor)
	}
}

func TestHandleAppendEntriesRejectsPrevLogMismatch(t *testing.T) {
	n := newTestNode(0, 3)
	n.currentTerm = 2
	n.log = append(n.log, LogEntry{Term: 1, Index: 1})

	var reply AppendEntriesReply
	n.HandleAppendEntries(&AppendEntriesArgs{
		Term:         2,
		LeaderId:     1,
		PrevLogIndex: 2,
		PrevLogTerm:  2,
	}, &reply)

	if reply.Success {
		t.Fatalf("expected append rejection on prev log mismatch")
	}
	if len(n.log) != 2 {
		t.Fatalf("expected log to remain unchanged, len=%d", len(n.log))
	}
}

func TestHandleAppendEntriesTruncatesConflictAndUpdatesCommit(t *testing.T) {
	n := newTestNode(0, 3)
	n.currentTerm = 2
	n.state = Candidate
	n.log = []LogEntry{
		{Term: 0, Index: 0},
		{Term: 1, Index: 1, Command: Command{Type: "put", Key: "a", Value: "1"}},
		{Term: 2, Index: 2, Command: Command{Type: "put", Key: "b", Value: "2"}},
	}

	var reply AppendEntriesReply
	n.HandleAppendEntries(&AppendEntriesArgs{
		Term:         3,
		LeaderId:     1,
		PrevLogIndex: 1,
		PrevLogTerm:  1,
		Entries: []LogEntry{
			{Term: 3, Index: 2, Command: Command{Type: "put", Key: "b", Value: "new"}},
			{Term: 3, Index: 3, Command: Command{Type: "put", Key: "c", Value: "3"}},
		},
		LeaderCommit: 3,
	}, &reply)

	if !reply.Success {
		t.Fatalf("expected append success")
	}
	if n.currentTerm != 3 {
		t.Fatalf("expected term=3 got %d", n.currentTerm)
	}
	if n.state != Follower {
		t.Fatalf("expected follower state, got %v", n.state)
	}
	if n.LeaderID() != 1 {
		t.Fatalf("expected leader id 1 got %d", n.LeaderID())
	}
	if len(n.log) != 4 {
		t.Fatalf("expected log len=4 got %d", len(n.log))
	}
	if n.log[2].Term != 3 || n.log[2].Command.Value != "new" {
		t.Fatalf("expected conflicted entry replaced at index 2")
	}
	if n.log[3].Term != 3 || n.log[3].Command.Key != "c" {
		t.Fatalf("expected new entry appended at index 3")
	}
	if n.commitIndex != 3 {
		t.Fatalf("expected commitIndex=3 got %d", n.commitIndex)
	}
}

func TestTryAdvanceCommitIndexRequiresMajorityAndCurrentTerm(t *testing.T) {
	n := newTestNode(0, 3)
	n.currentTerm = 2
	n.log = []LogEntry{
		{Term: 0, Index: 0},
		{Term: 1, Index: 1},
		{Term: 2, Index: 2},
		{Term: 1, Index: 3},
	}
	n.commitIndex = 0
	n.matchIndex = map[int]int{0: 0, 1: 2, 2: 1}

	n.tryAdvanceCommitIndex()
	if n.commitIndex != 2 {
		t.Fatalf("expected commitIndex=2 got %d", n.commitIndex)
	}

	n2 := newTestNode(0, 3)
	n2.currentTerm = 2
	n2.log = []LogEntry{
		{Term: 0, Index: 0},
		{Term: 1, Index: 1},
		{Term: 1, Index: 2},
	}
	n2.commitIndex = 0
	n2.matchIndex = map[int]int{0: 0, 1: 2, 2: 2}

	n2.tryAdvanceCommitIndex()
	if n2.commitIndex != 0 {
		t.Fatalf("expected commitIndex to stay 0 when no current-term entry has majority, got %d", n2.commitIndex)
	}
}

func TestSubmitLeaderOnlyAndAppend(t *testing.T) {
	n := newTestNode(0, 1)

	if _, _, ok := n.Submit(Command{Type: "put", Key: "k", Value: "v"}); ok {
		t.Fatalf("expected submit to fail for non-leader")
	}

	n.state = Leader
	n.currentTerm = 5
	n.becomeLeaderLocked()
	if n.LeaderID() != 0 {
		t.Fatalf("expected leader id 0 got %d", n.LeaderID())
	}

	index, term, ok := n.Submit(Command{Type: "put", Key: "k", Value: "v"})
	if !ok {
		t.Fatalf("expected leader submit to succeed")
	}
	if index != 1 || term != 5 {
		t.Fatalf("unexpected submit return values index=%d term=%d", index, term)
	}
	if len(n.log) != 2 {
		t.Fatalf("expected log len=2 got %d", len(n.log))
	}
	if n.log[1].Command.Key != "k" || n.log[1].Command.Value != "v" {
		t.Fatalf("unexpected command appended: %+v", n.log[1].Command)
	}
}

func TestSendHeartbeatsStepDownOnHigherTermReply(t *testing.T) {
	n := newTestNode(0, 2)
	n.state = Leader
	n.currentTerm = 4
	n.log = []LogEntry{{Term: 0, Index: 0}}
	n.nextIndex = map[int]int{0: 1, 1: 1}
	n.matchIndex = map[int]int{0: 0, 1: 0}
	n.rpcHandler = &mockRPCHandler{
		appendEntriesFn: func(ctx context.Context, target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error {
			reply.Term = 5
			reply.Success = false
			return nil
		},
	}

	n.replicateToFollower(1, "peer-1")

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.state != Follower {
		t.Fatalf("expected follower state got %v", n.state)
	}
	if n.currentTerm != 5 {
		t.Fatalf("expected term=5 got %d", n.currentTerm)
	}
	if n.LeaderID() != -1 {
		t.Fatalf("expected leader id reset after stepping down, got %d", n.LeaderID())
	}
}
