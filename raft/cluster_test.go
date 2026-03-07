package raft

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type inMemoryRPCNetwork struct {
	mu      sync.RWMutex
	nodes   map[string]*Node
	offline map[string]bool
	drop    map[string]map[string]bool
}

func newInMemoryRPCNetwork() *inMemoryRPCNetwork {
	return &inMemoryRPCNetwork{
		nodes:   make(map[string]*Node),
		offline: make(map[string]bool),
		drop:    make(map[string]map[string]bool),
	}
}

func (n *inMemoryRPCNetwork) register(name string, node *Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes[name] = node
}

func (n *inMemoryRPCNetwork) setOffline(name string, offline bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.offline[name] = offline
}

func (n *inMemoryRPCNetwork) setDrop(from, to string, drop bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.drop[from]; !ok {
		n.drop[from] = make(map[string]bool)
	}
	n.drop[from][to] = drop
}

func (n *inMemoryRPCNetwork) shouldDrop(from, to string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.offline[from] || n.offline[to] {
		return true
	}
	return n.drop[from][to]
}

func (n *inMemoryRPCNetwork) node(name string) *Node {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.nodes[name]
}

type inMemoryRPCHandler struct {
	net  *inMemoryRPCNetwork
	from string
}

func (h *inMemoryRPCHandler) RequestVote(ctx context.Context, target string, args *RequestVoteArgs, reply *RequestVoteReply) error {
	if h.net.shouldDrop(h.from, target) {
		return errors.New("unreachable")
	}
	node := h.net.node(target)
	if node == nil {
		return errors.New("target not found")
	}
	localReply := &RequestVoteReply{}
	node.HandleRequestVote(args, localReply)
	*reply = *localReply
	return nil
}

func (h *inMemoryRPCHandler) AppendEntries(ctx context.Context, target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error {
	if h.net.shouldDrop(h.from, target) {
		return errors.New("unreachable")
	}
	node := h.net.node(target)
	if node == nil {
		return errors.New("target not found")
	}
	localReply := &AppendEntriesReply{}
	node.HandleAppendEntries(args, localReply)
	*reply = *localReply
	return nil
}

type raftCluster struct {
	peers   []string
	nodes   []*Node
	applyCh []chan ApplyMsg
	net     *inMemoryRPCNetwork
}

func newRaftCluster(t *testing.T, size int) *raftCluster {
	t.Helper()

	peers := make([]string, size)
	for i := range peers {
		peers[i] = fmt.Sprintf("n%d", i)
	}

	net := newInMemoryRPCNetwork()
	c := &raftCluster{
		peers:   peers,
		nodes:   make([]*Node, size),
		applyCh: make([]chan ApplyMsg, size),
		net:     net,
	}

	for i := 0; i < size; i++ {
		applyCh := make(chan ApplyMsg, 1024)
		handler := &inMemoryRPCHandler{net: net, from: peers[i]}
		node := NewNode(i, peers, applyCh, handler)
		net.register(peers[i], node)
		c.nodes[i] = node
		c.applyCh[i] = applyCh
	}

	t.Cleanup(func() {
		c.shutdown()
	})

	return c
}

func (c *raftCluster) shutdown() {
	for i, node := range c.nodes {
		if node == nil {
			continue
		}
		c.net.setOffline(c.peers[i], true)
		node.Shutdown()
	}
	// Give goroutines a chance to observe shutdown signals.
	time.Sleep(30 * time.Millisecond)
}

func (c *raftCluster) crash(id int) {
	if id < 0 || id >= len(c.nodes) || c.nodes[id] == nil {
		return
	}
	c.net.setOffline(c.peers[id], true)
	c.nodes[id].Shutdown()
}

func (c *raftCluster) waitForLeader(timeout time.Duration) (int, int, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		maxTerm := -1
		leaderIDs := make([]int, 0)

		for i, node := range c.nodes {
			if node == nil || c.net.shouldDrop(c.peers[i], c.peers[i]) {
				continue
			}
			term, isLeader := node.GetState()
			if !isLeader {
				continue
			}
			if term > maxTerm {
				maxTerm = term
				leaderIDs = leaderIDs[:0]
				leaderIDs = append(leaderIDs, i)
			} else if term == maxTerm {
				leaderIDs = append(leaderIDs, i)
			}
		}

		if maxTerm >= 0 && len(leaderIDs) == 1 {
			return leaderIDs[0], maxTerm, true
		}

		time.Sleep(25 * time.Millisecond)
	}
	return -1, -1, false
}

func waitForCondition(timeout time.Duration, predicate func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestElectionSafetyAtMostOneLeaderPerTerm(t *testing.T) {
	c := newRaftCluster(t, 5)

	leaderByTerm := make(map[int]int)
	observedLeader := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for id, node := range c.nodes {
			term, isLeader := node.GetState()
			if !isLeader {
				continue
			}
			observedLeader = true
			if existing, ok := leaderByTerm[term]; ok && existing != id {
				t.Fatalf("election safety violated: term %d had leaders %d and %d", term, existing, id)
			}
			leaderByTerm[term] = id
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !observedLeader {
		t.Fatalf("expected to observe at least one elected leader")
	}
}

func TestLeaderCompletenessAcrossLeaderChange(t *testing.T) {
	c := newRaftCluster(t, 5)

	leaderID, term, ok := c.waitForLeader(5 * time.Second)
	if !ok {
		t.Fatalf("failed to elect initial leader")
	}

	cmd := Command{Type: "put", Key: "k", Value: "v1"}
	index, _, isLeader := c.nodes[leaderID].Submit(cmd)
	if !isLeader {
		t.Fatalf("expected node %d to be leader", leaderID)
	}

	committed := waitForCondition(4*time.Second, func() bool {
		c.nodes[leaderID].mu.RLock()
		defer c.nodes[leaderID].mu.RUnlock()
		return c.nodes[leaderID].commitIndex >= index
	})
	if !committed {
		t.Fatalf("entry did not commit on leader")
	}

	c.crash(leaderID)

	newLeaderID, newTerm, ok := c.waitForLeader(5 * time.Second)
	if !ok {
		t.Fatalf("failed to elect new leader after crash")
	}
	if newTerm <= term {
		t.Fatalf("expected higher term after leader crash, old=%d new=%d", term, newTerm)
	}

	newLeader := c.nodes[newLeaderID]
	newLeader.mu.RLock()
	defer newLeader.mu.RUnlock()
	if len(newLeader.log) <= index {
		t.Fatalf("new leader log too short: len=%d index=%d", len(newLeader.log), index)
	}
	if newLeader.log[index].Command != cmd {
		t.Fatalf("leader completeness violated: expected %+v got %+v", cmd, newLeader.log[index].Command)
	}
}

func TestLogMatchingAndConflictRepair(t *testing.T) {
	leader := newTestNode(0, 2)
	follower := newTestNode(1, 2)

	leader.state = Leader
	leader.currentTerm = 3
	leader.log = []LogEntry{
		{Term: 0, Index: 0},
		{Term: 1, Index: 1, Command: Command{Type: "put", Key: "a", Value: "1"}},
		{Term: 2, Index: 2, Command: Command{Type: "put", Key: "b", Value: "2"}},
		{Term: 3, Index: 3, Command: Command{Type: "put", Key: "c", Value: "3"}},
	}
	leader.nextIndex = map[int]int{0: len(leader.log), 1: len(leader.log)}
	leader.matchIndex = map[int]int{0: len(leader.log) - 1, 1: 0}

	follower.currentTerm = 3
	follower.log = []LogEntry{
		{Term: 0, Index: 0},
		{Term: 1, Index: 1, Command: Command{Type: "put", Key: "a", Value: "1"}},
		{Term: 1, Index: 2, Command: Command{Type: "put", Key: "x", Value: "old"}},
		{Term: 1, Index: 3, Command: Command{Type: "put", Key: "y", Value: "stale"}},
	}

	leader.rpcHandler = &mockRPCHandler{
		appendEntriesFn: func(ctx context.Context, target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error {
			follower.HandleAppendEntries(args, reply)
			return nil
		},
	}

	stepDownCh := make(chan struct{}, 1)

	before := make([]LogEntry, len(leader.log))
	copy(before, leader.log)

	leader.sendHeartbeats(stepDownCh)
	time.Sleep(80 * time.Millisecond)
	if leader.nextIndex[1] >= len(leader.log) {
		t.Fatalf("expected nextIndex decrement on initial mismatch")
	}

	ok := waitForCondition(2*time.Second, func() bool {
		leader.sendHeartbeats(stepDownCh)
		time.Sleep(40 * time.Millisecond)
		leader.mu.RLock()
		defer leader.mu.RUnlock()
		follower.mu.RLock()
		defer follower.mu.RUnlock()
		if len(follower.log) != len(leader.log) {
			return false
		}
		for i := range leader.log {
			if leader.log[i] != follower.log[i] {
				return false
			}
		}
		return true
	})
	if !ok {
		t.Fatalf("follower log did not converge to leader log")
	}

	if len(leader.log) != len(before) {
		t.Fatalf("leader append-only violated: log length changed from %d to %d", len(before), len(leader.log))
	}
	for i := range before {
		if leader.log[i] != before[i] {
			t.Fatalf("leader append-only violated: entry at %d changed", i)
		}
	}
}

func TestStateMachineSafetyNoConflictingAppliedIndex(t *testing.T) {
	c := newRaftCluster(t, 5)

	leaderID, _, ok := c.waitForLeader(5 * time.Second)
	if !ok {
		t.Fatalf("failed to elect initial leader")
	}

	for i := 0; i < 4; i++ {
		_, _, isLeader := c.nodes[leaderID].Submit(Command{Type: "put", Key: fmt.Sprintf("k%d", i), Value: fmt.Sprintf("v%d", i)})
		if !isLeader {
			t.Fatalf("lost leadership while submitting initial commands")
		}
	}

	// Force a leader change and continue submitting commands.
	c.crash(leaderID)
	newLeaderID, _, ok := c.waitForLeader(5 * time.Second)
	if !ok {
		t.Fatalf("failed to elect replacement leader")
	}

	for i := 4; i < 8; i++ {
		_, _, isLeader := c.nodes[newLeaderID].Submit(Command{Type: "put", Key: fmt.Sprintf("k%d", i), Value: fmt.Sprintf("v%d", i)})
		if !isLeader {
			t.Fatalf("replacement leader lost leadership during submits")
		}
	}

	appliedByIndex := make(map[int]Command)
	seenByIndex := make(map[int]map[int]bool)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		for nodeID, ch := range c.applyCh {
			for {
				select {
				case msg := <-ch:
					existing, ok := appliedByIndex[msg.Index]
					if ok && existing != msg.Command {
						t.Fatalf("state machine safety violated at index %d: %+v vs %+v", msg.Index, existing, msg.Command)
					}
					appliedByIndex[msg.Index] = msg.Command
					if _, ok := seenByIndex[msg.Index]; !ok {
						seenByIndex[msg.Index] = make(map[int]bool)
					}
					seenByIndex[msg.Index][nodeID] = true
				default:
					goto nextNode
				}
			}
		nextNode:
		}
		time.Sleep(15 * time.Millisecond)
	}

	if len(appliedByIndex) == 0 {
		t.Fatalf("expected applied entries but found none")
	}

	sharedIndexes := 0
	for _, nodes := range seenByIndex {
		if len(nodes) >= 2 {
			sharedIndexes++
		}
	}
	if sharedIndexes == 0 {
		t.Fatalf("expected at least one index applied on multiple nodes")
	}
}

func TestCommitAdvancesOnlyAfterMajorityReplication(t *testing.T) {
	leader := newTestNode(0, 5)
	leader.state = Leader
	leader.currentTerm = 3
	leader.log = []LogEntry{
		{Term: 0, Index: 0},
		{Term: 3, Index: 1, Command: Command{Type: "put", Key: "k", Value: "v"}},
	}
	leader.commitIndex = 0
	leader.nextIndex = map[int]int{0: 2, 1: 2, 2: 2, 3: 2, 4: 2}
	leader.matchIndex = map[int]int{0: 1, 1: 0, 2: 0, 3: 0, 4: 0}

	successPeers := map[int]bool{1: true}
	leader.rpcHandler = &mockRPCHandler{
		appendEntriesFn: func(ctx context.Context, target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error {
			id, err := strconv.Atoi(strings.TrimPrefix(target, "peer-"))
			if err != nil {
				id = -1
			}
			reply.Term = args.Term
			reply.Success = successPeers[id]
			return nil
		},
	}

	stepDownCh := make(chan struct{}, 1)
	leader.sendHeartbeats(stepDownCh)
	time.Sleep(100 * time.Millisecond)

	if leader.commitIndex != 0 {
		t.Fatalf("commit index advanced without majority, commitIndex=%d", leader.commitIndex)
	}

	successPeers[2] = true
	leader.sendHeartbeats(stepDownCh)
	time.Sleep(100 * time.Millisecond)

	if leader.commitIndex != 1 {
		t.Fatalf("expected commit index to advance with majority, got %d", leader.commitIndex)
	}
}

func TestFollowerAppliesCommittedEntriesInOrder(t *testing.T) {
	n := newTestNode(0, 1)
	done := make(chan struct{})
	go func() {
		n.applyCommittedEntries()
		close(done)
	}()

	args := &AppendEntriesArgs{
		Term:         2,
		LeaderId:     1,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []LogEntry{
			{Term: 2, Index: 1, Command: Command{Type: "put", Key: "a", Value: "1"}},
			{Term: 2, Index: 2, Command: Command{Type: "put", Key: "b", Value: "2"}},
			{Term: 2, Index: 3, Command: Command{Type: "put", Key: "c", Value: "3"}},
		},
		LeaderCommit: 3,
	}
	reply := &AppendEntriesReply{}
	n.HandleAppendEntries(args, reply)
	if !reply.Success {
		t.Fatalf("expected append success")
	}

	got := make([]ApplyMsg, 0, 3)
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < 3 && time.Now().Before(deadline) {
		select {
		case msg := <-n.applyCh:
			got = append(got, msg)
		case <-time.After(50 * time.Millisecond):
		}
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 apply messages got %d", len(got))
	}
	if got[0].Index != 1 || got[1].Index != 2 || got[2].Index != 3 {
		t.Fatalf("expected in-order apply indexes [1 2 3], got [%d %d %d]", got[0].Index, got[1].Index, got[2].Index)
	}

	n.Shutdown()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("apply loop did not stop on shutdown")
	}
}

func TestElectionConvergenceAndLeaderFailoverTiming(t *testing.T) {
	const trials = 5
	convergence := make([]time.Duration, 0, trials)
	failovers := make([]time.Duration, 0, trials)

	for i := 0; i < trials; i++ {
		c := newRaftCluster(t, 5)

		start := time.Now()
		leaderID, _, ok := c.waitForLeader(5 * time.Second)
		if !ok {
			t.Fatalf("trial %d: failed to elect leader", i)
		}
		convergence = append(convergence, time.Since(start))

		failoverStart := time.Now()
		c.crash(leaderID)
		newLeaderID, _, ok := c.waitForLeader(5 * time.Second)
		if !ok {
			t.Fatalf("trial %d: failed to elect replacement leader", i)
		}
		if newLeaderID == leaderID {
			t.Fatalf("trial %d: expected different replacement leader", i)
		}
		failovers = append(failovers, time.Since(failoverStart))

		c.shutdown()
	}

	sort.Slice(convergence, func(i, j int) bool { return convergence[i] < convergence[j] })
	sort.Slice(failovers, func(i, j int) bool { return failovers[i] < failovers[j] })

	medianConvergence := convergence[len(convergence)/2]
	medianFailover := failovers[len(failovers)/2]
	maxFailover := failovers[len(failovers)-1]

	t.Logf("leader election convergence median=%v", medianConvergence)
	t.Logf("leader failover median=%v max=%v", medianFailover, maxFailover)

	if maxFailover > 4*time.Second {
		t.Fatalf("failover too slow: max=%v", maxFailover)
	}
	if medianConvergence > 2*time.Second {
		t.Fatalf("convergence too slow: median=%v", medianConvergence)
	}
}
