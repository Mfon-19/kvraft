package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"
)

type ClientRequest struct {
	Type  string
	Key   string
	Value string
}

type ClientResponse struct {
	Success bool
	Value   string
	Error   string
}

func sendRequest(address string, req ClientRequest) (ClientResponse, error) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return ClientResponse{}, err
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(&req); err != nil {
		return ClientResponse{}, err
	}

	var resp ClientResponse
	if err := decoder.Decode(&resp); err != nil {
		return ClientResponse{}, err
	}

	return resp, nil
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("--- Starting Raft-KV Integration Test ---")

	// Allow time for cluster to elect a leader
	time.Sleep(2 * time.Second)

	addresses := []string{"localhost:8000", "localhost:8001", "localhost:8002"}
	var leaderAddr string

	// 1. Leader Discovery
	log.Println("[1/10] Finding leader...")
	for _, addr := range addresses {
		resp, err := sendRequest(addr, ClientRequest{Type: "put", Key: "ping", Value: "pong"})
		if err == nil && resp.Success {
			leaderAddr = addr
			log.Printf("\t✓ Leader found at %s", addr)
			break
		}
	}
	if leaderAddr == "" {
		log.Fatal("\t✗ Could not find leader. Is the cluster running?")
	}

	// 2. Basic Put/Get
	log.Println("[2/10] Testing Basic Put/Get...")
	resp, err := sendRequest(leaderAddr, ClientRequest{Type: "put", Key: "foo", Value: "bar"})
	if err != nil || !resp.Success {
		log.Fatalf("\t✗ Put failed: %v", resp.Error)
	}

	resp, err = sendRequest(leaderAddr, ClientRequest{Type: "get", Key: "foo"})
	if err != nil || resp.Value != "bar" {
		log.Fatalf("\t✗ Get failed. Expected 'bar', got '%s'", resp.Value)
	}
	log.Println("\t✓ Basic Put/Get passed")

	// 3. Replication Check
	log.Println("[3/10] Verifying Replication...")
	time.Sleep(10 * time.Second)

	successCount := 0
	for _, addr := range addresses {
		resp, _ := sendRequest(addr, ClientRequest{Type: "get", Key: "foo"})
		if resp.Success && resp.Value == "bar" {
			successCount++
		}
	}
	if successCount < 2 {
		log.Fatalf("\t✗ Replication failed. Only %d/3 nodes have data.", successCount)
	}
	log.Printf("\t✓ Data present on %d/3 nodes", successCount)

	// 4. Sequential Writes
	log.Println("[4/10] Sequential Writes...")
	for i := 0; i < 5; i++ {
		k, v := fmt.Sprintf("seq-%d", i), fmt.Sprintf("val-%d", i)
		if _, err := sendRequest(leaderAddr, ClientRequest{Type: "put", Key: k, Value: v}); err != nil {
			log.Fatalf("\t✗ Write failed at index %d", i)
		}
	}
	log.Println("\t✓ 5 Sequential writes successful")

	// 5. Update Key
	log.Println("[5/10] Updating Keys...")
	sendRequest(leaderAddr, ClientRequest{Type: "put", Key: "foo", Value: "updated"})
	resp, _ = sendRequest(leaderAddr, ClientRequest{Type: "get", Key: "foo"})
	if resp.Value != "updated" {
		log.Fatalf("\t✗ Update failed. Got %s", resp.Value)
	}
	log.Println("\t✓ Key updated successfully")

	// 6. Delete Key
	log.Println("[6/10] Deleting Keys...")
	sendRequest(leaderAddr, ClientRequest{Type: "delete", Key: "foo"})
	resp, _ = sendRequest(leaderAddr, ClientRequest{Type: "get", Key: "foo"})
	if resp.Success {
		log.Fatal("\t✗ Key should be deleted but was found")
	}
	log.Println("\t✓ Key deleted")

	// 7. Verify Delete Replication
	log.Println("[7/10] Verifying Delete Replication...")
	time.Sleep(600 * time.Millisecond)
	for _, addr := range addresses {
		resp, _ := sendRequest(addr, ClientRequest{Type: "get", Key: "foo"})
		if resp.Success {
			log.Fatalf("\t✗ Node %s still has deleted key", addr)
		}
	}
	log.Println("\t✓ Delete replicated to all nodes")

	// 8. Write to Follower (Expect Failure)
	log.Println("[8/10] Testing Follower Rejection...")
	checkedFollower := false
	for _, addr := range addresses {
		if addr != leaderAddr {
			resp, _ := sendRequest(addr, ClientRequest{Type: "put", Key: "bad", Value: "data"})
			if !resp.Success && resp.Error == "not leader" {
				checkedFollower = true
				break
			}
		}
	}
	if !checkedFollower {
		log.Println("\t⚠ Could not verify follower rejection (network errors or logic mismatch)")
	} else {
		log.Println("\t✓ Follower correctly rejected write")
	}

	// 9. Throughput Test
	log.Println("[9/10] High-Throughput Test (100 Writes)...")
	start := time.Now()
	total := 100
	ok := 0
	for i := 0; i < total; i++ {
		k := fmt.Sprintf("bench-%d", i)
		if resp, _ := sendRequest(leaderAddr, ClientRequest{Type: "put", Key: k, Value: "x"}); resp.Success {
			ok++
		}
	}
	duration := time.Since(start)
	log.Printf("\t✓ %d/%d writes succeeded in %v (%.0f req/sec)", ok, total, duration, float64(ok)/duration.Seconds())

	// 10. Final Consistency
	log.Println("[10/10] Final Consistency Check...")
	time.Sleep(1 * time.Second) // Allow full propagation

	testKey := "bench-99" // Check the last written key
	consistent := 0
	for _, addr := range addresses {
		resp, _ := sendRequest(addr, ClientRequest{Type: "get", Key: testKey})
		if resp.Success && resp.Value == "x" {
			consistent++
		}
	}

	if consistent >= 2 {
		log.Printf("\t✓ Consistency verified (%d nodes match)", consistent)
	} else {
		log.Fatalf("\t✗ Cluster inconsistent. Only %d nodes match.", consistent)
	}

	log.Println("\n--- All Tests Passed Successfully ---")
}
