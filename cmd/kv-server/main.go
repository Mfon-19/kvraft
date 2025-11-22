package main

import (
	"flag"
	"fmt"
	"kvraft/server"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	// command line flags
	id := flag.Int("id", 0, "Node Id")
	port := flag.Int("port", 5000, "Raft RPC port")
	clientPort := flag.Int("client-port", 8000, "Client request port")
	peersFlag := flag.String("peers", "", "Comma-separated list of peer addresses (e.g., localhost:5001,localhost:5002)")
	flag.Parse()

	if *id < 0 {
		log.Fatal("Node Id must be >= 0")
	}

	// parse peers
	var peers []string
	if *peersFlag != "" {
		peers = strings.Split(*peersFlag, ",")
	}

	address := fmt.Sprintf("localhost:%d", *port)
	clientAddress := fmt.Sprintf("localhost:%d", *clientPort)

	log.Printf("Starting Raft-KV node %d", *id)
	log.Printf("Raft RPC address: %s", address)
	log.Printf("Client address: %s", clientAddress)
	log.Printf("Peers: %v", peers)

	srv := server.NewRaftKVServer(*id, address, peers)

	if err := srv.Start(); err != nil {
		log.Fatalf("Failed to start Raft server: %v", err)
	}

	if err := srv.StartClientListener(clientAddress); err != nil {
		log.Printf("Failed to start client listener: %v", err)
		log.Printf("Cleaning up and shutting down...")
		srv.Close()
		os.Exit(1)
	}

	log.Printf("Node %d is running", *id)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("Shutting down node %d", *id)
	srv.Close()
}
