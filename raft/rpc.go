package raft

import (
	pb "kvraft/proto"
	"time"
)

type RequestVoteArgs struct {
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

type RequestVoteReply struct {
	Term        int
	VoteGranted bool
}

type AppendEntriesArgs struct {
	Term         int
	LeaderId     int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term    int
	Success bool
}

type LogEntry struct {
	Term    int
	Index   int
	Command Command
}

type Command struct {
	Type  string
	Key   string
	Value string
}

type ApplyMsg struct {
	Index   int
	Command Command
}

// NodeState The state of a Raft node. Can be in either one of the three states
type NodeState int

const (
	Follower NodeState = iota
	Candidate
	Leader
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

const (
	HeartbeatInterval  = 150 * time.Millisecond
	ElectionTimeoutMin = 300 * time.Millisecond
	ElectionTimeoutMax = 600 * time.Millisecond
)

func RequestVoteArgsToProto(args *RequestVoteArgs) *pb.RequestVoteRequest {
	return &pb.RequestVoteRequest{
		Term:         int32(args.Term),
		CandidateId:  int32(args.CandidateId),
		LastLogIndex: int32(args.LastLogIndex),
		LastLogTerm:  int32(args.LastLogTerm),
	}
}

func RequestVoteReplyFromProto(reply *pb.RequestVoteResponse) *RequestVoteReply {
	return &RequestVoteReply{
		Term:        int(reply.Term),
		VoteGranted: reply.VoteGranted,
	}
}

func RequestVoteArgsFromProto(req *pb.RequestVoteRequest) *RequestVoteArgs {
	return &RequestVoteArgs{
		Term:         int(req.Term),
		CandidateId:  int(req.CandidateId),
		LastLogIndex: int(req.LastLogIndex),
		LastLogTerm:  int(req.LastLogTerm),
	}
}

func RequestVoteReplyToProto(reply *RequestVoteReply) *pb.RequestVoteResponse {
	return &pb.RequestVoteResponse{
		Term:        int32(reply.Term),
		VoteGranted: reply.VoteGranted,
	}
}

func AppendEntriesArgsToProto(args *AppendEntriesArgs) *pb.AppendEntriesRequest {
	entries := make([]*pb.LogEntry, len(args.Entries))
	for i, entry := range args.Entries {
		entries[i] = LogEntryToProto(&entry)
	}
	return &pb.AppendEntriesRequest{
		Term:         int32(args.Term),
		LeaderId:     int32(args.LeaderId),
		PrevLogIndex: int32(args.PrevLogIndex),
		PrevLogTerm:  int32(args.PrevLogTerm),
		Entries:      entries,
		LeaderCommit: int32(args.LeaderCommit),
	}
}

func AppendEntriesReplyFromProto(reply *pb.AppendEntriesResponse) *AppendEntriesReply {
	return &AppendEntriesReply{
		Term:    int(reply.Term),
		Success: reply.Success,
	}
}

func AppendEntriesArgsFromProto(req *pb.AppendEntriesRequest) *AppendEntriesArgs {
	entries := make([]LogEntry, len(req.Entries))
	for i, entry := range req.Entries {
		entries[i] = *LogEntryFromProto(entry)
	}
	return &AppendEntriesArgs{
		Term:         int(req.Term),
		LeaderId:     int(req.LeaderId),
		PrevLogIndex: int(req.PrevLogIndex),
		PrevLogTerm:  int(req.PrevLogTerm),
		Entries:      entries,
		LeaderCommit: int(req.LeaderCommit),
	}
}

func AppendEntriesReplyToProto(reply *AppendEntriesReply) *pb.AppendEntriesResponse {
	return &pb.AppendEntriesResponse{
		Term:    int32(reply.Term),
		Success: reply.Success,
	}
}

func LogEntryToProto(entry *LogEntry) *pb.LogEntry {
	return &pb.LogEntry{
		Term:  int32(entry.Term),
		Index: int32(entry.Index),
		Command: &pb.Command{
			Type:  entry.Command.Type,
			Key:   entry.Command.Key,
			Value: entry.Command.Value,
		},
	}
}

func LogEntryFromProto(entry *pb.LogEntry) *LogEntry {
	return &LogEntry{
		Term:  int(entry.Term),
		Index: int(entry.Index),
		Command: Command{
			Type:  entry.Command.Type,
			Key:   entry.Command.Key,
			Value: entry.Command.Value,
		},
	}
}
