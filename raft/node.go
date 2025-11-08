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
			n.mu.Unlock()
			n.state = Follower
			// n.runFollower()
			return
		}

		if n.state != Candidate || n.currentTerm != currentTerm {
			return
		}

		if r.VoteGranted {
			votes++
		}

		if votes >= votesNeeded {
			n.state = Leader
			n.becomeLeader()
			log.Printf("[Node %d] Won election for term %d", n.id, currentTerm)
		}

		n.resetElectionTimer()

		select {
		case <-n.electionTimer.C:
		// jitter + exponential backoff + run new election
		case <-n.heartbeatCh:
			// received AppendEntries from leader
			n.mu.Lock()
			n.state = Follower
			n.mu.Unlock()
		case <-n.shutdownCh:
			return
		}
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

	go n.sendHeartbeats()
}

func (n *Node) runLeader() {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	go n.applyCommittedEntries()

	select {
	case <-ticker.C:
		n.sendHeartbeats()
	case <-n.shutdownCh:
		return
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

func (n *Node) sendHeartbeats() {
	n.mu.RLock()
	// can't send heartbeats if node isn't leader
	if n.state != Leader {
		n.mu.RUnlock()
		return
	}

	currentTerm := n.currentTerm
	leaderId := n.id
	commitIndex := n.commitIndex
	n.mu.RUnlock()

	replies := make([]AppendEntriesReply, len(n.peers))

	var wg sync.WaitGroup
	for i, peer := range n.peers {
		if i == n.id {
			continue
		}

		// a goroutine for each peer. this also guarantees the log matching property
		wg.Add(1)
		go func(peerIdx int, peerAddr string) {
			defer wg.Done()

			backoff := 10 * time.Millisecond
			for {
				n.mu.RLock()
				if n.state != Leader || n.currentTerm != currentTerm {
					n.mu.RUnlock()
					return
				}
				nextIndex := n.nextIndex[peerIdx]

				prevLogIndex := nextIndex - 1
				var prevLogTerm int
				if prevLogIndex >= 0 && prevLogIndex < len(n.log) {
					prevLogTerm = n.log[prevLogIndex].Term
				} else {
					prevLogTerm = 0
					if prevLogIndex < 0 {
						prevLogIndex = -1
					}
				}

				// get log entries to send
				var entries []LogEntry
				if nextIndex >= 0 && nextIndex < len(n.log) {
					entries = append([]LogEntry(nil), n.log[nextIndex:]...)
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

				ctx, cancel := context.WithTimeout(context.Background(), n.rpcTimeout)
				var reply AppendEntriesReply
				err := n.rpcHandler.AppendEntries(ctx, peerAddr, args, &replies[peerIdx])
				cancel()
				if err != nil {
					time.Sleep(backoff)
					// exponential backoff
					if backoff < 500*time.Millisecond {
						backoff *= 2
					}
					log.Printf("[Node %d] AppendEntries rpc to %s failed", n.id, peerAddr)
					continue
				}

				// if higher term discovered, step down
				if reply.Term > currentTerm {
					n.mu.Lock()
					if reply.Term > n.currentTerm {
						n.currentTerm = reply.Term
						n.votedFor = -1
						n.state = Follower
					}
					n.mu.Unlock()
					return
				}

				if reply.Success {
					n.mu.Lock()

					match := prevLogIndex + len(entries)
					if match < 0 {
						match = prevLogIndex
					}
					// update the matchIndex and nextIndex for this peer
					n.matchIndex[peerIdx] = max(n.matchIndex[peerIdx], match)
					n.nextIndex[peerIdx] = n.matchIndex[peerIdx] + 1

					// advance the commit index
					n.tryAdvanceCommitIndex()
					n.mu.Unlock()
					return
				}

				// here, the log matching property is implemented. we don't use accelerated
				// backtracking, so this will be O(n) in the worst case which is fine
				n.mu.Lock()
				if n.nextIndex[peerIdx] > 1 {
					n.nextIndex[peerIdx]--
				}
				n.mu.Unlock()
			}

		}(i, peer)
	}

	wg.Wait()
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
