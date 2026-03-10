package raft

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

type Node struct {
	mu           sync.RWMutex
	shutdownOnce sync.Once

	// server identity and peer addresses
	id       int
	peers    []string
	leaderID int

	// persistent state for each raft node
	currentTerm int
	votedFor    int
	log         []LogEntry

	// volatile state for each raft node
	commitIndex int
	lastApplied int
	state       NodeState

	// leader state only used when node is the leader
	nextIndex  map[int]int
	matchIndex map[int]int

	// channels
	applyCh chan ApplyMsg
	// commitNotifyCh coalesces commit-index advances so the applier can wake without polling.
	commitNotifyCh chan struct{}
	heartbeatCh    chan bool
	voteCh         chan bool
	shutdownCh     chan struct{}
	electionTimer  *time.Timer

	// One long-lived replication worker per follower. Workers are signaled on
	// submit and heartbeat ticks instead of spawning per-tick goroutines.
	replicationTrigger map[int]chan struct{}

	rpcHandler RPCHandler
	rpcTimeout time.Duration
}

type RPCHandler interface {
	RequestVote(ctx context.Context, target string, args *RequestVoteArgs, reply *RequestVoteReply) error
	AppendEntries(ctx context.Context, target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error
}

func NewNode(id int, peers []string, applyCh chan ApplyMsg, rpcHandler RPCHandler) *Node {
	n := &Node{
		id:                 id,
		peers:              peers,
		leaderID:           -1,
		currentTerm:        0,
		votedFor:           -1,
		log:                make([]LogEntry, 1),
		commitIndex:        0,
		lastApplied:        0,
		state:              Follower,
		applyCh:            applyCh,
		commitNotifyCh:     make(chan struct{}, 1),
		heartbeatCh:        make(chan bool, 100),
		voteCh:             make(chan bool, 100),
		shutdownCh:         make(chan struct{}),
		replicationTrigger: make(map[int]chan struct{}),
		rpcHandler:         rpcHandler,
		rpcTimeout:         2 * time.Second,
	}

	// dummy entry
	n.log[0] = LogEntry{Term: 0, Index: 0}

	for i, peer := range peers {
		if i == id {
			continue
		}
		ch := make(chan struct{}, 1)
		n.replicationTrigger[i] = ch
		go n.replicationWorker(i, peer, ch)
	}

	go n.applyCommittedEntries()
	go n.run()

	return n
}

func (n *Node) run() {
	n.resetElectionTimer()

	for {
		select {
		case <-n.shutdownCh:
			return
		default:
		}

		n.mu.RLock()
		state := n.state
		n.mu.RUnlock()

		switch state {
		case Follower:
			n.runFollower()
		case Leader:
			n.runLeader()
		case Candidate:
			n.runCandidate()
		}
	}
}

func (n *Node) runFollower() {
	n.resetElectionTimer()

	select {
	case <-n.heartbeatCh:
		n.resetElectionTimer()
	case <-n.voteCh:
		n.resetElectionTimer()
	case <-n.electionTimer.C:
		n.mu.Lock()
		n.state = Candidate
		n.mu.Unlock()
	case <-n.shutdownCh:
		return
	}
}

func (n *Node) runCandidate() {
	n.mu.Lock()

	// prerequisites for a new candidate:
	// increase current term and vote for self.
	n.currentTerm++
	n.votedFor = n.id
	n.leaderID = -1
	currentTerm := n.currentTerm
	lastLogIndex := len(n.log) - 1
	lastLogTerm := n.log[lastLogIndex].Term

	args := &RequestVoteArgs{
		Term:         currentTerm,
		CandidateId:  n.id,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	}
	n.mu.Unlock()

	slog.Debug("raft election started", "node_id", n.id, "term", currentTerm)

	voteCh := make(chan RequestVoteReply, len(n.peers))

	for i, peer := range n.peers {
		if i == n.id {
			continue
		}

		go func(peerAddr string) {
			var reply RequestVoteReply
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()

			err := n.rpcHandler.RequestVote(ctx, peerAddr, args, &reply)
			if err == nil {
				voteCh <- reply
				return
			}
			slog.Debug("raft request vote failed", "node_id", n.id, "peer", peerAddr, "error", err)
		}(peer)
	}

	votes := 1
	votesNeeded := (len(n.peers)+1)/2 + 1
	n.resetElectionTimer()

	for {
		select {
		case r := <-voteCh:
			if r.Term > currentTerm {
				n.mu.Lock()
				n.becomeFollowerLocked(r.Term)
				n.mu.Unlock()
				return
			}
			if r.VoteGranted {
				votes++
			}
			if votes >= votesNeeded {
				n.mu.Lock()
				if n.state == Candidate && n.currentTerm == currentTerm {
					n.becomeLeaderLocked()
					slog.Info("raft leader elected", "node_id", n.id, "term", currentTerm)
					n.mu.Unlock()
					n.signalAllReplications()
					return
				}
				n.mu.Unlock()
			}
		case <-n.electionTimer.C:
			return
		case <-n.shutdownCh:
			return
		}
	}
}

func (n *Node) runLeader() {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	n.signalAllReplications()

	for {
		select {
		case <-n.shutdownCh:
			return
		case <-ticker.C:
			n.mu.RLock()
			isLeader := n.state == Leader
			n.mu.RUnlock()
			if !isLeader {
				return
			}
			n.signalAllReplications()
		}
	}
}

func (n *Node) becomeLeaderLocked() {
	n.state = Leader
	n.leaderID = n.id
	n.nextIndex = make(map[int]int, len(n.peers))
	n.matchIndex = make(map[int]int, len(n.peers))

	lastLogIndex := len(n.log) - 1
	for i := range n.peers {
		n.nextIndex[i] = lastLogIndex + 1
		n.matchIndex[i] = 0
	}
	n.matchIndex[n.id] = lastLogIndex
}

func (n *Node) becomeFollowerLocked(term int) {
	if term > n.currentTerm {
		n.currentTerm = term
	}
	n.votedFor = -1
	n.state = Follower
	n.leaderID = -1
}

func (n *Node) replicationWorker(peerIdx int, peerAddr string, trigger <-chan struct{}) {
	for {
		select {
		case <-n.shutdownCh:
			return
		case <-trigger:
			for {
				if !n.replicateToFollower(peerIdx, peerAddr) {
					break
				}
			}
		}
	}
}

func (n *Node) signalAllReplications() {
	n.mu.RLock()
	if n.state != Leader {
		n.mu.RUnlock()
		return
	}
	chs := make([]chan struct{}, 0, len(n.replicationTrigger))
	for _, ch := range n.replicationTrigger {
		chs = append(chs, ch)
	}
	n.mu.RUnlock()

	for _, ch := range chs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *Node) replicateToFollower(peerIdx int, peerAddr string) bool {
	n.mu.RLock()
	if n.state != Leader {
		n.mu.RUnlock()
		return false
	}

	currentTerm := n.currentTerm
	leaderID := n.id
	commitIndex := n.commitIndex
	nextIdx := n.nextIndex[peerIdx]
	prevLogIndex := nextIdx - 1
	if prevLogIndex < 0 || prevLogIndex >= len(n.log) {
		n.mu.RUnlock()
		return false
	}
	prevLogTerm := n.log[prevLogIndex].Term

	entries := make([]LogEntry, 0)
	if nextIdx < len(n.log) {
		entries = append(entries, n.log[nextIdx:]...)
	}
	n.mu.RUnlock()

	args := &AppendEntriesArgs{
		Term:         currentTerm,
		LeaderId:     leaderID,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: commitIndex,
	}

	var reply AppendEntriesReply
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := n.rpcHandler.AppendEntries(ctx, peerAddr, args, &reply)
	cancel()
	if err != nil {
		return false
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if reply.Term > n.currentTerm {
		n.becomeFollowerLocked(reply.Term)
		return false
	}
	if n.state != Leader || n.currentTerm != currentTerm {
		return false
	}

	if reply.Success {
		match := prevLogIndex + len(entries)
		if match > n.matchIndex[peerIdx] {
			n.matchIndex[peerIdx] = match
			n.nextIndex[peerIdx] = match + 1
			n.tryAdvanceCommitIndexLocked()
		}
		return n.nextIndex[peerIdx] < len(n.log)
	}

	if n.nextIndex[peerIdx] > 1 {
		n.nextIndex[peerIdx]--
		return true
	}
	return false
}

func (n *Node) tryAdvanceCommitIndex() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tryAdvanceCommitIndexLocked()
}

func (n *Node) tryAdvanceCommitIndexLocked() {
	for N := len(n.log) - 1; N > n.commitIndex; N-- {
		if n.log[N].Term != n.currentTerm {
			continue
		}

		count := 1 // self
		for i := range n.peers {
			if i == n.id {
				continue
			}
			if n.matchIndex[i] >= N {
				count++
			}
		}

		if count > len(n.peers)/2 {
			n.commitIndex = N
			n.notifyCommitAdvance()
			break
		}
	}
}

func (n *Node) notifyCommitAdvance() {
	select {
	case n.commitNotifyCh <- struct{}{}:
	default:
	}
}

func (n *Node) applyCommittedEntries() {
	for {
		select {
		case <-n.shutdownCh:
			return
		default:
		}

		n.mu.Lock()
		if n.lastApplied < n.commitIndex {
			n.lastApplied++
			entry := n.log[n.lastApplied]
			n.mu.Unlock()

			n.applyCh <- ApplyMsg{
				Index:   entry.Index,
				Term:    entry.Term,
				Command: entry.Command,
			}
			continue
		}
		n.mu.Unlock()

		select {
		case <-n.shutdownCh:
			return
		case <-n.commitNotifyCh:
		}
	}
}

func (n *Node) resetElectionTimer() {
	timeout := ElectionTimeoutMin + time.Duration(rand.Int63n(int64(ElectionTimeoutMax-ElectionTimeoutMin)))
	if n.electionTimer == nil {
		n.electionTimer = time.NewTimer(timeout)
		return
	}

	// Stop/drain/reset avoids stale timer signals racing with a fresh election timeout.
	if !n.electionTimer.Stop() {
		select {
		case <-n.electionTimer.C:
		default:
		}
	}
	n.electionTimer.Reset(timeout)
}

func (n *Node) HandleRequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	n.mu.Lock()
	defer n.mu.Unlock()

	reply.Term = n.currentTerm
	reply.VoteGranted = false

	if n.currentTerm > args.Term {
		return
	}

	if args.Term > n.currentTerm {
		n.becomeFollowerLocked(args.Term)
	}

	lastLogIndex := len(n.log) - 1
	lastLogTerm := n.log[lastLogIndex].Term

	logOk := (args.LastLogTerm > lastLogTerm) || (args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex)
	if (n.votedFor == -1 || n.votedFor == args.CandidateId) && logOk {
		n.votedFor = args.CandidateId
		reply.VoteGranted = true
		select {
		case n.voteCh <- true:
		default:
		}
	}
}

func (n *Node) HandleAppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	n.mu.Lock()
	defer n.mu.Unlock()

	reply.Term = n.currentTerm
	reply.Success = false

	if n.currentTerm > args.Term {
		return
	}

	if args.Term >= n.currentTerm {
		n.becomeFollowerLocked(args.Term)
		n.leaderID = args.LeaderId
	}

	select {
	case n.heartbeatCh <- true:
	default:
	}

	if args.PrevLogIndex < 0 {
		reply.Success = true
	} else if args.PrevLogIndex >= len(n.log) || n.log[args.PrevLogIndex].Term != args.PrevLogTerm {
		return
	}

	for i, entry := range args.Entries {
		idx := args.PrevLogIndex + i + 1
		if idx < len(n.log) {
			if n.log[idx].Term != entry.Term {
				n.log = n.log[:idx]
				n.log = append(n.log, entry)
			}
			continue
		}
		n.log = append(n.log, entry)
	}

	if args.LeaderCommit > n.commitIndex {
		n.commitIndex = min(args.LeaderCommit, len(n.log)-1)
		n.notifyCommitAdvance()
	}

	reply.Success = true
}

func (n *Node) Submit(cmd Command) (int, int, bool) {
	n.mu.Lock()
	if n.state != Leader {
		n.mu.Unlock()
		return -1, -1, false
	}

	index := len(n.log)
	term := n.currentTerm
	entry := LogEntry{
		Term:    term,
		Index:   index,
		Command: cmd,
	}
	n.log = append(n.log, entry)
	n.matchIndex[n.id] = index
	n.mu.Unlock()

	n.signalAllReplications()
	return index, term, true
}

func (n *Node) IsLeader() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.state == Leader
}

func (n *Node) GetState() (int, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.currentTerm, n.state == Leader
}

func (n *Node) LeaderID() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.leaderID
}

func (n *Node) Shutdown() {
	n.shutdownOnce.Do(func() {
		if n.electionTimer != nil {
			n.electionTimer.Stop()
		}
		close(n.shutdownCh)
	})
}
