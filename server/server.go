package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
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

const commandTypeReadBarrier = "read_barrier"
const maxCompletedNotifications = 1024

type Config struct {
	ID            int
	RaftAddress   string
	ClientAddress string
	Peers         []string
	ClientPeers   []string

	StoreDir     string
	StoreOptions kvstore.OpenOptions

	ApplyBuffer   int
	CommitTimeout time.Duration
}

type pendingKey struct {
	index int
	term  int
}

type raftBackend interface {
	HandleRequestVote(args *raft.RequestVoteArgs, reply *raft.RequestVoteReply)
	HandleAppendEntries(args *raft.AppendEntriesArgs, reply *raft.AppendEntriesReply)
	Submit(cmd raft.Command) (int, int, bool)
	IsLeader() bool
	LeaderID() int
	Shutdown()
}

type RaftKVServer struct {
	id            int
	address       string
	clientAddress string

	raftNode raftBackend
	store    *kvstore.DB

	grpcServer *grpc.Server
	clientGRPC *grpc.Server
	listener   net.Listener
	clientLn   net.Listener

	pendingLock sync.Mutex
	pending     map[pendingKey]chan error
	completed   map[pendingKey]error
	completionQ []pendingKey

	commitTimeout time.Duration
	clientPeers   map[int]string
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
		clientAddress: cfg.ClientAddress,
		store:         open,
		pending:       make(map[pendingKey]chan error),
		completed:     make(map[pendingKey]error),
		commitTimeout: cfg.CommitTimeout,
		clientPeers:   addressesByID(cfg.ClientPeers),
		closedCh:      make(chan struct{}),
	}
	if cfg.ClientAddress != "" {
		s.clientPeers[cfg.ID] = cfg.ClientAddress
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
	if len(cfg.ClientPeers) > 0 && len(cfg.ClientPeers) != len(cfg.Peers) {
		return cfg, fmt.Errorf("client peer count (%d) must match peer count (%d)", len(cfg.ClientPeers), len(cfg.Peers))
	}
	if cfg.ClientAddress == "" && cfg.ID < len(cfg.ClientPeers) {
		cfg.ClientAddress = strings.TrimSpace(cfg.ClientPeers[cfg.ID])
	}
	if len(cfg.ClientPeers) == 0 && len(cfg.Peers) > 0 && cfg.ClientAddress != "" {
		derived, err := deriveClientPeerAddresses(cfg.Peers, cfg.RaftAddress, cfg.ClientAddress)
		if err != nil {
			return cfg, err
		}
		cfg.ClientPeers = derived
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

func deriveClientPeerAddresses(raftPeers []string, raftAddress, clientAddress string) ([]string, error) {
	_, raftPortText, err := net.SplitHostPort(raftAddress)
	if err != nil {
		return nil, fmt.Errorf("parse raft address %q: %w", raftAddress, err)
	}
	raftPort, err := strconv.Atoi(raftPortText)
	if err != nil {
		return nil, fmt.Errorf("parse raft port %q: %w", raftPortText, err)
	}

	_, clientPortText, err := net.SplitHostPort(clientAddress)
	if err != nil {
		return nil, fmt.Errorf("parse client address %q: %w", clientAddress, err)
	}
	clientPort, err := strconv.Atoi(clientPortText)
	if err != nil {
		return nil, fmt.Errorf("parse client port %q: %w", clientPortText, err)
	}

	offset := clientPort - raftPort
	clientPeers := make([]string, len(raftPeers))
	for i, peer := range raftPeers {
		host, peerPortText, err := net.SplitHostPort(strings.TrimSpace(peer))
		if err != nil {
			return nil, fmt.Errorf("parse peer address %q: %w", peer, err)
		}
		peerPort, err := strconv.Atoi(peerPortText)
		if err != nil {
			return nil, fmt.Errorf("parse peer port %q: %w", peerPortText, err)
		}
		clientPeerPort := peerPort + offset
		if clientPeerPort <= 0 {
			return nil, fmt.Errorf("derived invalid client port %d from peer %q", clientPeerPort, peer)
		}
		clientPeers[i] = net.JoinHostPort(host, strconv.Itoa(clientPeerPort))
	}
	return clientPeers, nil
}

func addressesByID(addresses []string) map[int]string {
	byID := make(map[int]string, len(addresses))
	for i, address := range addresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		byID[i] = address
	}
	return byID
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
			case commandTypeReadBarrier:
				// Replicated read barriers do not mutate state; they only order reads
				// after the leader has committed and applied all prior entries.
			}

			key := pendingKey{index: msg.Index, term: msg.Term}
			s.pendingLock.Lock()
			if ch, ok := s.pending[key]; ok {
				ch <- applyErr
				delete(s.pending, key)
			} else {
				if len(s.completionQ) >= maxCompletedNotifications {
					evict := s.completionQ[0]
					s.completionQ = s.completionQ[1:]
					delete(s.completed, evict)
				}
				s.completed[key] = applyErr
				s.completionQ = append(s.completionQ, key)
			}
			s.pendingLock.Unlock()
		}
	}
}

func (s *RaftKVServer) Get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := s.withCommitTimeout(ctx)
	defer cancel()

	if err := s.submitAndWait(ctx, raft.Command{Type: commandTypeReadBarrier}); err != nil {
		return nil, err
	}
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

func (s *RaftKVServer) withCommitTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), s.commitTimeout)
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.commitTimeout)
}

func (s *RaftKVServer) submitAndWait(ctx context.Context, cmd raft.Command) error {
	if s.raftNode == nil {
		return ErrNotLeader
	}

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
	if err, ok := s.completed[waitKey]; ok {
		delete(s.completed, waitKey)
		s.pendingLock.Unlock()
		return err
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
	if s.raftNode == nil {
		return false
	}
	return s.raftNode.IsLeader()
}

func (s *RaftKVServer) leaderAddress() string {
	if s.raftNode == nil {
		return ""
	}

	leaderID := s.raftNode.LeaderID()
	if leaderID < 0 {
		return ""
	}
	if address, ok := s.clientPeers[leaderID]; ok {
		return address
	}
	if leaderID == s.id {
		return s.clientAddress
	}
	return ""
}

func (s *RaftKVServer) errorResponse(err error) *pb.KVResponse {
	resp := &pb.KVResponse{Success: false, Error: err.Error()}
	if errors.Is(err, ErrNotLeader) {
		resp.Leader = s.leaderAddress()
	}
	return resp
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
	value, err := g.server.Get(ctx, req.Key)
	if err != nil {
		return g.server.errorResponse(err), nil
	}
	return &pb.KVResponse{Success: true, Value: string(value)}, nil
}

func (g *GRPCKVService) Put(ctx context.Context, req *pb.KVRequest) (*pb.KVResponse, error) {
	if err := g.server.Put(req.Key, req.Value); err != nil {
		return g.server.errorResponse(err), nil
	}
	return &pb.KVResponse{Success: true}, nil
}

func (g *GRPCKVService) Delete(ctx context.Context, req *pb.KVRequest) (*pb.KVResponse, error) {
	if err := g.server.Delete(req.Key); err != nil {
		return g.server.errorResponse(err), nil
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
	s.clientAddress = clientPort
	if s.clientPeers == nil {
		s.clientPeers = make(map[int]string)
	}
	s.clientPeers[s.id] = clientPort
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
