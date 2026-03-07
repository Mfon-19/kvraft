package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"kvraft/common"
	pb "kvraft/proto"
)

func main() {
	address := flag.String("address", "localhost:8000", "Server address")
	flag.Parse()

	conn, err := grpc.Dial(*address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect to server at %s: %v\n", *address, err)
	}
	defer conn.Close()
	client := pb.NewKVServiceClient(conn)

	fmt.Println("╔════════════════════════════════════════════════════╗")
	fmt.Println("║         Raft-KV Client (with gRPC backend)         ║")
	fmt.Println("╚════════════════════════════════════════════════════╝")
	fmt.Printf("\nConnected to: %s\n", *address)
	fmt.Println("\nAvailable commands:")
	fmt.Println("  put <key> <value>  - Store a key-value pair")
	fmt.Println("  get <key>          - Retrieve a value by key")
	fmt.Println("  delete <key>       - Delete a key")
	fmt.Println("  help               - Show this help message")
	fmt.Println("  quit / exit        - Exit the client")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}

		cmd := parts[0]
		switch cmd {
		case "quit", "exit":
			fmt.Println("Goodbye!")
			return
		case "help":
			fmt.Println("\nAvailable commands:")
			fmt.Println("  put <key> <value>  - Store a key-value pair")
			fmt.Println("  get <key>          - Retrieve a value by key")
			fmt.Println("  delete <key>       - Delete a key")
			fmt.Println("  help               - Show this help message")
			fmt.Println("  quit / exit        - Exit the client")
			fmt.Println()
		case "put":
			if len(parts) != 3 {
				fmt.Println("Usage: put <key> <value>")
				continue
			}
			resp, err := invoke(client, common.ClientRequest{Type: common.OpPut, Key: parts[1], Value: parts[2]})
			if err != nil {
				log.Printf("RPC error: %v", err)
				return
			}
			if resp.Success {
				fmt.Println("✓ OK")
			} else {
				fmt.Printf("✗ Error: %s\n", resp.Error)
				if resp.Error == "not leader" {
					fmt.Println("  (Try connecting to a different node)")
				}
			}
		case "get":
			if len(parts) != 2 {
				fmt.Println("Usage: get <key>")
				continue
			}
			resp, err := invoke(client, common.ClientRequest{Type: common.OpGet, Key: parts[1]})
			if err != nil {
				log.Printf("RPC error: %v", err)
				return
			}
			if resp.Success {
				fmt.Printf("%s\n", resp.Value)
			} else {
				fmt.Printf("✗ Error: %s\n", resp.Error)
			}
		case "delete":
			if len(parts) != 2 {
				fmt.Println("Usage: delete <key>")
				continue
			}
			resp, err := invoke(client, common.ClientRequest{Type: common.OpDelete, Key: parts[1]})
			if err != nil {
				log.Printf("RPC error: %v", err)
				return
			}
			if resp.Success {
				fmt.Println("✓ OK")
			} else {
				fmt.Printf("✗ Error: %s\n", resp.Error)
				if resp.Error == "not leader" {
					fmt.Println("  (Try connecting to a different node)")
				}
			}
		default:
			fmt.Printf("Unknown command: %s\n", cmd)
			fmt.Println("Type 'help' for available commands")
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading input: %v", err)
	}
}

func invoke(client pb.KVServiceClient, req common.ClientRequest) (common.ClientResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return common.InvokeKV(ctx, client, req)
}
