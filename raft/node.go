package raft

import (
	"math/rand"
	"sync"
	"time"
)

type Node struct {
	mu sync.RWMutex

	// server identity and peer addresses
	id    int
	peers []string

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
	applyCh       chan ApplyMsg
	heartbeatCh   chan bool
	voteCh        chan bool
	shutdownCh    chan struct{}
	electionTimer *time.Timer

	rpcHandler RPCHandler
}

type RPCHandler interface {
	RequestVote(target string, args *RequestVoteArgs, reply *RequestVoteReply) error
	AppendEntries(target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error
}

func NewNode(id int, peers []string, applyCh chan ApplyMsg, rpcHandler RPCHandler) *Node {
	n := &Node{
		id:          id,
		peers:       peers,
		currentTerm: 0,
		votedFor:    -1, // voted for no one yet
		log:         make([]LogEntry, 1),
		commitIndex: 0,
		lastApplied: 0,
		state:       Follower, // every raft node starts out as a follower
		applyCh:     applyCh,
		heartbeatCh: make(chan bool, 100),
		voteCh:      make(chan bool, 100),
		shutdownCh:  make(chan struct{}),
		rpcHandler:  rpcHandler,
	}

	// dummy entry
	n.log[0] = LogEntry{Term: 0, Index: 0}

	go n.run()
	return n
}

func (n *Node) run() {
	n.resetElectionTimer()
}

func (n *Node) runFollower() {
	select {
	case <-n.heartbeatCh:
		n.resetElectionTimer()
	case <-n.voteCh:
		n.resetElectionTimer()
	case <-n.electionTimer.C:
		// election timeout. become a candidate
		n.mu.Lock()
		n.state = Candidate
		n.mu.Unlock()
	case <-n.shutdownCh:
		return
	}
}

func (n *Node) resetElectionTimer() {
	timeout := ElectionTimeoutMin + time.Duration(rand.Int63n(int64(ElectionTimeoutMax-ElectionTimeoutMin)))

	if n.electionTimer == nil {
		n.electionTimer = time.NewTimer(timeout)
	} else {
		n.electionTimer.Stop()
		n.electionTimer.Reset(timeout)
	}
}
