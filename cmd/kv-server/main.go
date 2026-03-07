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

func main() {
	id := flag.Int("id", 0, "Node Id")
	port := flag.Int("port", 6000, "Raft RPC port")
	clientPort := flag.Int("client-port", 8000, "Client request port")
	peersFlag := flag.String("peers", "", "Comma-separated list of peer addresses (e.g., localhost:5001,localhost:5002)")
	logLevel := flag.String("log-level", "info", "Log level: debug|info|warn|error")
	logFormat := flag.String("log-format", "text", "Log format: text|json")
	flag.Parse()

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

	address := fmt.Sprintf("localhost:%d", *port)
	clientAddress := fmt.Sprintf("localhost:%d", *clientPort)

	slog.Info("starting raft-kv node", "id", *id, "raft_address", address, "client_address", clientAddress, "peers", peers)

	srv, err := server.NewRaftKVServer(server.Config{
		ID:          *id,
		RaftAddress: address,
		Peers:       peers,
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
