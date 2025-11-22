package raft

import (
	"context"
	"log"
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
	rpcTimeout time.Duration
}

type RPCHandler interface {
	RequestVote(ctx context.Context, target string, args *RequestVoteArgs, reply *RequestVoteReply) error
	AppendEntries(ctx context.Context, target string, args *AppendEntriesArgs, reply *AppendEntriesReply) error
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
		rpcTimeout:  2 * time.Second,
	}

	// dummy entry
	n.log[0] = LogEntry{Term: 0, Index: 0}

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

func (n *Node) runCandidate() {
	n.mu.Lock()

	// prerequisites for a new candidate.
	// increase current term, vote for self
	n.currentTerm++
	n.votedFor = n.id
	currentTerm := n.currentTerm
	lastLogIndex := len(n.log) - 1
	lastLogTerm := n.log[lastLogIndex].Term
	n.mu.Unlock()

	log.Printf("[Node %d] Starting election for term %d", n.id, currentTerm)

	votes := 1
	// needs a majority vote
	votesNeeded := (len(n.peers)+1)/2 + 1

	replies := make([]RequestVoteReply, len(n.peers))
	args := &RequestVoteArgs{
		Term:         currentTerm,
		CandidateId:  n.id,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	}

	var wg sync.WaitGroup
	for i, peer := range n.peers {
		if i == n.id {
			continue
		}

		wg.Add(1)
		go func(peerAddr string, index int) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), n.rpcTimeout)
			defer cancel()

			err := n.rpcHandler.RequestVote(ctx, peerAddr, args, &replies[index])
			if err != nil {
				log.Printf("[Node %d] Error requesting vote from %s", n.id, peerAddr)
				return
			}
		}(peer, i)
	}

	wg.Wait()

	// Count votes
	for i, r := range replies {
		if i == n.id {
			continue
		}

		// if we find a peer with a higher term,
		// revert back to follower
		if r.Term > currentTerm {
			n.mu.Lock()
			n.currentTerm = r.Term
			n.votedFor = -1
			n.state = Follower
			n.mu.Unlock()
			return
		}

		if r.VoteGranted {
			votes++
		}
	}

	// Check if we won
	n.mu.Lock()
	if n.state == Candidate && n.currentTerm == currentTerm && votes >= votesNeeded {
		n.state = Leader
		log.Printf("[Node %d] is now the LEADER for term %d", n.id, currentTerm)
		n.mu.Unlock()
		n.becomeLeader()
	} else {
		n.mu.Unlock()
	}
}

func (n *Node) becomeLeader() {
	n.nextIndex = make(map[int]int)
	n.matchIndex = make(map[int]int)

	lastLogIndex := len(n.log) - 1
	for i := range n.peers {
		n.nextIndex[i] = lastLogIndex + 1
		n.matchIndex[i] = 0
	}

	stepDownCh := make(chan struct{}, 1)
	go n.sendHeartbeats(stepDownCh)
}

func (n *Node) runLeader() {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	// indicates that this leader should step down
	// because a higher term was found
	stepDownCh := make(chan struct{}, 1)

	go n.sendHeartbeats(stepDownCh)

	go n.applyCommittedEntries()

	for {
		select {
		case <-n.shutdownCh:
			return
		case <-stepDownCh:
			return
		case <-ticker.C:
			go n.sendHeartbeats(stepDownCh)
		}
	}
}

func (n *Node) tryAdvanceCommitIndex() {
	for N := len(n.log) - 1; N > n.commitIndex; N-- {
		if n.log[N].Term != n.currentTerm {
			continue
		}

		// count self
		count := 1
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
			log.Printf("[Node %d] Advanced commitIndex to %d", n.id, n.commitIndex)
			break
		}
	}
}

func (n *Node) applyCommittedEntries() {
	for {
		n.mu.Lock()
		if n.lastApplied < n.commitIndex {
			n.lastApplied++
			entry := n.log[n.lastApplied]
			n.mu.Unlock()

			n.applyCh <- ApplyMsg{
				Index:   entry.Index,
				Command: entry.Command,
			}
		} else {
			n.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (n *Node) sendHeartbeats(stepDownCh chan struct{}) {
	n.mu.RLock()
	// can't send heartbeats if node isn't leader
	if n.state != Leader {
		n.mu.RUnlock()
		return
	}

	// snapshot current state
	currentTerm := n.currentTerm
	leaderId := n.id
	commitIndex := n.commitIndex
	n.mu.RUnlock()

	for i, peer := range n.peers {
		if i == n.id {
			continue
		}

		// a goroutine for each peer
		go func(peerIdx int, peerAddr string) {
			n.mu.Lock()
			// double check that we are still the leader
			if n.state != Leader {
				n.mu.RUnlock()
				return
			}

			nextIdx := n.nextIndex[peerIdx]
			prevLogIndex := nextIdx - 1
			prevLogTerm := 0

			if prevLogIndex >= 0 && prevLogIndex < len(n.log) {
				prevLogTerm = n.log[prevLogIndex].Term
			}

			// prepare entries to send
			var entries []LogEntry
			if nextIdx < len(n.log) {
				entries = n.log[nextIdx:]
			}
			n.mu.RUnlock()

			args := &AppendEntriesArgs{
				Term:         currentTerm,
				LeaderId:     leaderId,
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
				// we don't need to retry here. the ticker in runLeader() will retry heartbeats
				return
			}

			n.mu.Lock()
			defer n.mu.Unlock()

			// if reply has a higher term, become follower
			if reply.Term > currentTerm {
				if n.currentTerm != reply.Term {
					n.currentTerm = reply.Term
					n.votedFor = -1
					n.state = Follower

					// notify the runLeader() function to stop the loop
					select {
					case stepDownCh <- struct{}{}:
					default:
					}
				}
				return
			}

			// verify state hasn't changed while RPC was in flight
			if n.state != Leader || n.currentTerm != currentTerm {
				return
			}

			if reply.Success {
				// update indices
				match := prevLogIndex + len(entries)
				if match > n.matchIndex[peerIdx] {
					n.matchIndex[peerIdx] = match
					n.nextIndex[peerIdx] = match + 1
					n.tryAdvanceCommitIndex()
				}
			} else {
				// on log inconsistency, decrement nextIndex. since runLeader()
				// sends heartbeats at intervals, this meets the log matching property
				if n.nextIndex[peerIdx] > 1 {
					n.nextIndex[peerIdx]--
				}
			}
		}(i, peer)
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

func (n *Node) HandleRequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	n.mu.Lock()
	defer n.mu.Unlock()

	reply.Term = n.currentTerm
	reply.VoteGranted = false

	// don't grant vote if you have a higher term
	if n.currentTerm > args.Term {
		return
	}

	// update term if necessary
	if args.Term > n.currentTerm {
		n.currentTerm = args.Term
		n.votedFor = -1
		n.state = Follower
	}

	lastLogIndex := len(n.log) - 1
	lastLogTerm := n.log[lastLogIndex].Term

	// candidates log term must be at least as high as mine, and if equal their log
	// must be at least as up to date as mine
	logOk := (args.LastLogTerm > lastLogTerm) || (args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex)

	if (n.votedFor == -1 || n.votedFor == args.CandidateId) && logOk {
		n.votedFor = args.CandidateId
		reply.VoteGranted = true
		// Send to vote channel (non-blocking to avoid deadlock)
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

	// reject AppendEntries if leader term is not up to mine
	if n.currentTerm > args.Term {
		return
	}

	// update term if necessary, or step down if we're a leader/candidate
	if args.Term > n.currentTerm {
		n.currentTerm = args.Term
		n.state = Follower
		n.votedFor = -1
	} else if args.Term == n.currentTerm {
		// if we receive AppendEntries from leader with same term, step down
		if n.state != Follower {
			n.state = Follower
		}
	}

	// Send to heartbeat channel (non-blocking to avoid deadlock)
	select {
	case n.heartbeatCh <- true:
	default:
	}

	// if our logs don't match, tell the leader
	if args.PrevLogIndex < 0 {
		// Initial heartbeat case, PrevLogIndex = -1 is valid
		reply.Success = true
	} else if args.PrevLogIndex >= len(n.log) || n.log[args.PrevLogIndex].Term != args.PrevLogTerm {
		return
	}

	// append the entries to my log
	for i, entry := range args.Entries {
		idx := args.PrevLogIndex + i + 1
		if idx < len(n.log) {
			// if this entry is meant to match with the leader, discard faulty entries and append
			if n.log[idx].Term != entry.Term {
				n.log = n.log[:idx]
				n.log = append(n.log, entry)
			}
		} else {
			n.log = append(n.log, entry)
		}
	}

	// update commit index
	if args.LeaderCommit > n.commitIndex {
		n.commitIndex = min(args.LeaderCommit, len(n.log)-1)
	}

	// successfully appended
	reply.Success = true
}

func (n *Node) Submit(cmd Command) (int, int, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// can only submit to the leader
	if n.state != Leader {
		return -1, -1, false
	}

	// add this entry to our raft log
	index := len(n.log)
	entry := LogEntry{
		Term:    n.currentTerm,
		Index:   index,
		Command: cmd,
	}
	n.log = append(n.log, entry)

	log.Printf("[Node %d] Appended entry at %d: %+v", n.id, index, cmd)
	return index, n.currentTerm, true
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

func (n *Node) Shutdown() {
	close(n.shutdownCh)
}
