package server

import (
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/grpc"
	"kvraft/kvstore"
	pb "kvraft/proto"
	"kvraft/raft"
	"log"
	"net"
	"net/rpc"
	"sync"
	"time"
)

type RaftKVServer struct {
	mu sync.RWMutex

	id          int
	address     string
	raftNode    *raft.Node
	store       *kvstore.DB
	grpcServer  *grpc.Server
	listener    net.Listener
	pending     map[int]chan string
	pendingLock sync.Mutex
}

func NewRaftKVServer(id int, address string, peers []string) *RaftKVServer {
	s := &RaftKVServer{
		id:      id,
		address: address,
		pending: make(map[int]chan string),
	}

	open, err := kvstore.Open("newdir")
	if err != nil {
		return nil
	}
	s.store = open

	applyCh := make(chan raft.ApplyMsg, 100)
	rpcHandler := &RPCClient{}
	s.raftNode = raft.NewNode(id, peers, applyCh, rpcHandler)

	go s.applyCommittedEntries(applyCh)
	return nil
}

func (s *RaftKVServer) Start() error {
	// register rpc service
	s.rpcServer = rpc.NewServer()
	s.rpcServer.RegisterName("Raft", &RaftRPC{server: s})

	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return err
	}
	s.listener = listener

	log.Printf("[Server %d] Listening on %s", s.id, s.address)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.rpcServer.ServeConn(conn)
		}
	}()

	return nil
}

func (s *RaftKVServer) applyCommittedEntries(applyCh chan raft.ApplyMsg) {
	// loop over all commands waiting to be applied
	for msg := range applyCh {
		s.mu.Lock()

		// apply the message to our kv store
		switch msg.Command.Type {
		case "put":
			err := s.store.Put(msg.Command.Key, msg.Command.Value)
			if err != nil {
				log.Printf("[Server %d] Error applying PUT: %s = %s", s.id, msg.Command.Key, msg.Command.Value)
				return
			}
			log.Printf("[Server %d] Applied PUT: %s = %s", s.id, msg.Command.Key, msg.Command.Value)
		case "delete":
			err := s.store.Delete(msg.Command.Key)
			if err != nil {
				log.Printf("[Server %d] Error applying DELETE: %s", s.id, msg.Command.Key)
				return
			}
			log.Printf("[Server %d] Applied DELETE: %s", s.id, msg.Command.Key)
		}

		// notify the pending request
		s.pendingLock.Lock()
		if ch, ok := s.pending[msg.Index]; ok {
			ch <- msg.Command.Value
			delete(s.pending, msg.Index)
		}
		s.pendingLock.Unlock()

		s.mu.Unlock()
	}
}

// API for the client

func (s *RaftKVServer) Get(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.store.Get(key)
}

func (s *RaftKVServer) Put(key, value string) error {
	cmd := raft.Command{
		Type:  "put",
		Key:   key,
		Value: value,
	}

	index, _, isLeader := s.raftNode.Submit(cmd)
	if !isLeader {
		return fmt.Errorf("not leader")
	}

	ch := make(chan string, 1)
	s.pendingLock.Lock()
	s.pending[index] = ch
	s.pendingLock.Unlock()

	select {
	case <-ch:
		// command applied
		return nil
	}
}

func (s *RaftKVServer) Delete(key string) error {
	cmd := raft.Command{
		Type: "delete",
		Key:  key,
	}

	index, _, isLeader := s.raftNode.Submit(cmd)
	if !isLeader {
		return fmt.Errorf("not leader")
	}

	ch := make(chan string, 1)
	s.pendingLock.Lock()
	s.pending[index] = ch
	s.pendingLock.Unlock()

	select {
	case <-ch:
		return nil
	}
}

func (s *RaftKVServer) IsLeader() bool {
	return s.raftNode.IsLeader()
}

func (s *RaftKVServer) Close() {
	s.raftNode.Shutdown()
	if s.listener != nil {
		err := s.listener.Close()
		if err != nil {
			log.Printf("[Server %d] Error closing server", s.id)
			return
		}
	}
}

type GRPCRaftService struct {
	pb.UnimplementedRaftServiceServer
	server *RaftKVServer
}

func (g *GRPCRaftService) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	args := raft.RequestVoteArgsFromProto(req)
	reply := &raft.RequestVoteReply{}
	g.server.raftNode.HandleRequestVote(args, reply)
	return raft.RequestVoteReplyToProto(reply), nil
}

func (g *GRPCRaftService) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	args := raft.AppendEntriesArgsFromProto(req)
	reply := &raft.AppendEntriesReply{}
	g.server.raftNode.HandleAppendEntries(args, reply)
	return raft.AppendEntriesReplyToProto(reply), nil
}

type GRPCClient struct {
	peers []string
	conns map[string]*grpc.ClientConn
	mu    sync.Mutex
}

func (c *GRPCClient) getConnection(target string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conns == nil {
		c.conns = make(map[string]*grpc.ClientConn)
	}

	// if already connected, return connection
	if conn, ok := c.conns[target]; ok {
		return conn, nil
	}

	// if not connected, connect
	conn, err := grpc.Dial(target, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(2*time.Second))
	if err != nil {
		log.Printf("[GRPCClient] Error dialing server %s", target)
		return nil, err
	}

	c.conns[target] = conn
	return conn, nil
}

func (c *GRPCClient) RequestVote(ctx context.Context, target string, args *raft.RequestVoteArgs, reply *raft.RequestVoteReply) error {
	conn, err := c.getConnection(target)
	if err != nil {
		return err
	}

	client := pb.NewRaftServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	req := raft.RequestVoteArgsToProto(args)
	resp, err := client.RequestVote(ctx, req)
	if err != nil {
		return err
	}

	*reply = *raft.RequestVoteReplyFromProto(resp)
	return nil
}

func (c *GRPCClient) AppendEntries(ctx context.Context, target string, args *raft.AppendEntriesArgs, reply *raft.AppendEntriesReply) error {
	conn, err := c.getConnection(target)
	if err != nil {
		return err
	}

	client := pb.NewRaftServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	req := raft.AppendEntriesArgsToProto(args)
	resp, err := client.AppendEntries(ctx, req)
	if err != nil {
		return err
	}

	*reply = *raft.AppendEntriesReplyFromProto(resp)
	return nil
}

type ClientRequest struct {
	Type  string // "get", "put", "delete"
	Key   string
	Value string
}

type ClientResponse struct {
	Success bool
	Value   string
	Error   string
}

func (s *RaftKVServer) HandleClientRequest(req ClientRequest) ClientResponse {
	switch req.Type {
	case "get":
		value, err := s.Get(req.Key)
		if err != nil {
			return ClientResponse{Success: false, Error: err.Error()}
		}
		return ClientResponse{Success: true, Value: string(value)}

	case "put":
		err := s.Put(req.Key, req.Value)
		if err != nil {
			return ClientResponse{Success: false, Error: err.Error()}
		}
		return ClientResponse{Success: true}

	case "delete":
		err := s.Delete(req.Key)
		if err != nil {
			return ClientResponse{Success: false, Error: err.Error()}
		}
		return ClientResponse{Success: true}

	default:
		return ClientResponse{Success: false, Error: "unknown command"}
	}
}

func (s *RaftKVServer) StartClientListener(clientPort string) error {
	listener, err := net.Listen("tcp", clientPort)
	if err != nil {
		log.Printf("[Server %d] Error listening on port %s", s.id, clientPort)
		return err
	}

	log.Printf("[Server %d] Client listener on %s", s.id, clientPort)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("[Server %d] Error accepting client connection", s.id)
				return
			}
			// one goroutine per client connection
			go s.handleClientConn(conn)
		}
	}()

	return nil
}

func (s *RaftKVServer) handleClientConn(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		var req ClientRequest
		if err := decoder.Decode(&req); err != nil {
			return
		}

		resp := s.HandleClientRequest(req)
		if err := encoder.Encode(&resp); err != nil {
			return
		}
	}
}
