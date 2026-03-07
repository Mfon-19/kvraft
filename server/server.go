package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"kvraft/kvstore"
	pb "kvraft/proto"
	"kvraft/raft"
)

var (
	ErrNotLeader     = errors.New("not leader")
	ErrCommitTimeout = errors.New("command timed out waiting for commit")
	ErrServerClosed  = errors.New("server closed")
)

type Config struct {
	ID          int
	RaftAddress string
	Peers       []string

	StoreDir     string
	StoreOptions kvstore.OpenOptions

	ApplyBuffer   int
	CommitTimeout time.Duration
}

type pendingKey struct {
	index int
	term  int
}

type RaftKVServer struct {
	id      int
	address string

	raftNode *raft.Node
	store    *kvstore.DB

	grpcServer *grpc.Server
	clientGRPC *grpc.Server
	listener   net.Listener
	clientLn   net.Listener

	pendingLock sync.Mutex
	pending     map[pendingKey]chan error

	commitTimeout time.Duration
	closedCh      chan struct{}
	closeOnce     sync.Once
	applyWg       sync.WaitGroup
}

func NewRaftKVServer(cfg Config) (*RaftKVServer, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}

	open, err := kvstore.OpenWithOptions(cfg.StoreDir, cfg.StoreOptions)
	if err != nil {
		return nil, fmt.Errorf("open kvstore: %w", err)
	}

	s := &RaftKVServer{
		id:            cfg.ID,
		address:       cfg.RaftAddress,
		store:         open,
		pending:       make(map[pendingKey]chan error),
		commitTimeout: cfg.CommitTimeout,
		closedCh:      make(chan struct{}),
	}

	applyCh := make(chan raft.ApplyMsg, cfg.ApplyBuffer)
	rpcHandler := &GRPCClient{}
	s.raftNode = raft.NewNode(cfg.ID, cfg.Peers, applyCh, rpcHandler)

	s.applyWg.Add(1)
	go func() {
		defer s.applyWg.Done()
		s.applyCommittedEntries(applyCh)
	}()

	return s, nil
}

func normalizeConfig(cfg Config) (Config, error) {
	if cfg.ID < 0 {
		return cfg, fmt.Errorf("node id must be >= 0")
	}
	if cfg.RaftAddress == "" {
		return cfg, fmt.Errorf("raft address must be set")
	}
	if cfg.StoreDir == "" {
		cfg.StoreDir = fmt.Sprintf("kvstore_%d", cfg.ID)
	}
	if cfg.ApplyBuffer <= 0 {
		cfg.ApplyBuffer = 256
	}
	if cfg.CommitTimeout <= 0 {
		cfg.CommitTimeout = 5 * time.Second
	}
	if cfg.StoreOptions.MaxDataFileSizeBytes <= 0 {
		cfg.StoreOptions.MaxDataFileSizeBytes = 64 * 1024 * 1024
	}
	cfg.StoreOptions.ReadWrite = true
	return cfg, nil
}

func (s *RaftKVServer) Start() error {
	s.grpcServer = grpc.NewServer()
	pb.RegisterRaftServiceServer(s.grpcServer, &GRPCRaftService{server: s})

	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return err
	}
	s.listener = listener

	slog.Info("raft rpc listener started", "server_id", s.id, "address", s.address)
	go func() {
		if err := s.grpcServer.Serve(listener); err != nil {
			slog.Error("raft rpc server stopped", "server_id", s.id, "error", err)
		}
	}()

	return nil
}

func (s *RaftKVServer) applyCommittedEntries(applyCh <-chan raft.ApplyMsg) {
	for {
		select {
		case <-s.closedCh:
			return
		case msg := <-applyCh:
			var applyErr error
			switch msg.Command.Type {
			case "put":
				applyErr = s.store.Put(msg.Command.Key, msg.Command.Value)
			case "delete":
				applyErr = s.store.Delete(msg.Command.Key)
			}

			key := pendingKey{index: msg.Index, term: msg.Term}
			s.pendingLock.Lock()
			if ch, ok := s.pending[key]; ok {
				ch <- applyErr
				delete(s.pending, key)
			}
			s.pendingLock.Unlock()
		}
	}
}

func (s *RaftKVServer) Get(key string) ([]byte, error) {
	return s.store.Get(key)
}

func (s *RaftKVServer) Put(key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.commitTimeout)
	defer cancel()
	return s.submitAndWait(ctx, raft.Command{Type: "put", Key: key, Value: value})
}

func (s *RaftKVServer) Delete(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.commitTimeout)
	defer cancel()
	return s.submitAndWait(ctx, raft.Command{Type: "delete", Key: key})
}

func (s *RaftKVServer) submitAndWait(ctx context.Context, cmd raft.Command) error {
	index, term, isLeader := s.raftNode.Submit(cmd)
	if !isLeader {
		return ErrNotLeader
	}

	waitKey := pendingKey{index: index, term: term}
	waitCh := make(chan error, 1)

	s.pendingLock.Lock()
	select {
	case <-s.closedCh:
		s.pendingLock.Unlock()
		return ErrServerClosed
	default:
	}
	s.pending[waitKey] = waitCh
	s.pendingLock.Unlock()

	select {
	case err := <-waitCh:
		return err
	case <-ctx.Done():
		s.pendingLock.Lock()
		delete(s.pending, waitKey)
		s.pendingLock.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrCommitTimeout
		}
		return ctx.Err()
	case <-s.closedCh:
		s.pendingLock.Lock()
		delete(s.pending, waitKey)
		s.pendingLock.Unlock()
		return ErrServerClosed
	}
}

func (s *RaftKVServer) IsLeader() bool {
	return s.raftNode.IsLeader()
}

func (s *RaftKVServer) Close() {
	s.closeOnce.Do(func() {
		close(s.closedCh)

		s.pendingLock.Lock()
		for key, ch := range s.pending {
			ch <- ErrServerClosed
			delete(s.pending, key)
		}
		s.pendingLock.Unlock()

		if s.raftNode != nil {
			s.raftNode.Shutdown()
		}
		if s.grpcServer != nil {
			s.grpcServer.GracefulStop()
		}
		if s.clientGRPC != nil {
			s.clientGRPC.GracefulStop()
		}
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				slog.Error("failed to close raft listener", "server_id", s.id, "error", err)
			}
		}
		if s.clientLn != nil {
			if err := s.clientLn.Close(); err != nil {
				slog.Error("failed to close client listener", "server_id", s.id, "error", err)
			}
		}
		if s.store != nil {
			if err := s.store.Close(); err != nil {
				slog.Error("failed to close kvstore", "server_id", s.id, "error", err)
			}
		}

		s.applyWg.Wait()
	})
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

type GRPCKVService struct {
	pb.UnimplementedKVServiceServer
	server *RaftKVServer
}

func (g *GRPCKVService) Get(ctx context.Context, req *pb.KVRequest) (*pb.KVResponse, error) {
	value, err := g.server.Get(req.Key)
	if err != nil {
		return &pb.KVResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.KVResponse{Success: true, Value: string(value)}, nil
}

func (g *GRPCKVService) Put(ctx context.Context, req *pb.KVRequest) (*pb.KVResponse, error) {
	if err := g.server.Put(req.Key, req.Value); err != nil {
		return &pb.KVResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.KVResponse{Success: true}, nil
}

func (g *GRPCKVService) Delete(ctx context.Context, req *pb.KVRequest) (*pb.KVResponse, error) {
	if err := g.server.Delete(req.Key); err != nil {
		return &pb.KVResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.KVResponse{Success: true}, nil
}

type GRPCClient struct {
	conns map[string]*grpc.ClientConn
	mu    sync.Mutex
}

func (c *GRPCClient) getConnection(target string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conns == nil {
		c.conns = make(map[string]*grpc.ClientConn)
	}
	if conn, ok := c.conns[target]; ok {
		return conn, nil
	}

	conn, err := grpc.Dial(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Debug("grpc dial failed", "target", target, "error", err)
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
	resp, err := client.RequestVote(ctx, raft.RequestVoteArgsToProto(args))
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
	resp, err := client.AppendEntries(ctx, raft.AppendEntriesArgsToProto(args))
	if err != nil {
		return err
	}

	*reply = *raft.AppendEntriesReplyFromProto(resp)
	return nil
}

func (s *RaftKVServer) StartClientListener(clientPort string) error {
	listener, err := net.Listen("tcp", clientPort)
	if err != nil {
		return err
	}
	s.clientLn = listener
	s.clientGRPC = grpc.NewServer()
	pb.RegisterKVServiceServer(s.clientGRPC, &GRPCKVService{server: s})

	slog.Info("client listener started", "server_id", s.id, "address", clientPort)
	go func() {
		if err := s.clientGRPC.Serve(listener); err != nil {
			slog.Error("client grpc server stopped", "server_id", s.id, "error", err)
		}
	}()

	return nil
}
