package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"kvraft/common"
	"kvraft/server"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	id := flag.Int("id", 0, "Node Id")
	port := flag.Int("port", 6000, "Raft RPC port")
	clientPort := flag.Int("client-port", 8000, "Client request port")
	peersFlag := flag.String("peers", "", "Comma-separated list of peer addresses (e.g., localhost:5001,localhost:5002)")
	clientPeersFlag := flag.String("client-peers", "", "Comma-separated list of client addresses aligned with -peers (e.g., localhost:8000,localhost:8001)")
	logLevel := flag.String("log-level", "info", "Log level: debug|info|warn|error")
	logFormat := flag.String("log-format", "text", "Log format: text|json")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("caskv-server %s (commit=%s date=%s)\n", version, commit, date)
		return
	}

	if err := common.ConfigureLogger(*logLevel, *logFormat, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "logger configuration error: %v\n", err)
		os.Exit(1)
	}

	if *id < 0 {
		slog.Error("invalid node id", "id", *id)
		os.Exit(1)
	}

	var peers []string
	if *peersFlag != "" {
		peers = strings.Split(*peersFlag, ",")
	}
	var clientPeers []string
	if *clientPeersFlag != "" {
		clientPeers = strings.Split(*clientPeersFlag, ",")
	}

	address := fmt.Sprintf("localhost:%d", *port)
	clientAddress := fmt.Sprintf("localhost:%d", *clientPort)

	slog.Info("starting raft-kv node", "id", *id, "raft_address", address, "client_address", clientAddress, "peers", peers, "client_peers", clientPeers)

	srv, err := server.NewRaftKVServer(server.Config{
		ID:            *id,
		RaftAddress:   address,
		ClientAddress: clientAddress,
		Peers:         peers,
		ClientPeers:   clientPeers,
	})
	if err != nil {
		slog.Error("failed to construct raft server", "error", err)
		os.Exit(1)
	}

	if err := srv.Start(); err != nil {
		slog.Error("failed to start raft rpc server", "error", err)
		os.Exit(1)
	}

	if err := srv.StartClientListener(clientAddress); err != nil {
		slog.Error("failed to start client listener", "error", err)
		srv.Close()
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down raft-kv node", "id", *id)
	srv.Close()
}
