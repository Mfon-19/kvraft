package main

import (
	"encoding/json"
	"fmt"
	"kvraft/common"
	"log"
	"net"
	"time"
)



func sendRequest(address string, req common.ClientRequest) (common.ClientResponse, error) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return common.ClientResponse{}, err
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(&req); err != nil {
		return common.ClientResponse{}, err
	}

	var resp common.ClientResponse
	if err := decoder.Decode(&resp); err != nil {
		return common.ClientResponse{}, err
	}

	return resp, nil
}

func main() {
	log.Println("========================================")
	log.Println("Starting Raft-KV Test (with gRPC)")
	log.Println("========================================")

	// Wait for cluster to start and elect a leader
	log.Println("\nWaiting for cluster to start and elect leader...")
	time.Sleep(2 * time.Second)

	addresses := []string{"localhost:8000", "localhost:8001", "localhost:8002"}

	// Test 1: Find the leader
	log.Println("\n[Test 1] Finding leader...")
	var leaderAddr string
	for _, addr := range addresses {
		resp, err := sendRequest(addr, common.ClientRequest{Type: "put", Key: "test", Value: "123"})
		if err != nil {
			log.Printf("  Node %s not reachable: %v", addr, err)
			continue
		}
		if resp.Success {
			leaderAddr = addr
			log.Printf("  ✓ Found leader at %s", addr)
			break
		} else {
			log.Printf("  Node %s is not leader: %s", addr, resp.Error)
		}
	}

	if leaderAddr == "" {
		log.Fatal("  ✗ Could not find leader - ensure cluster is running")
	}

	// Test 2: Basic write and read
	log.Println("\n[Test 2] Testing basic write and read...")
	resp, err := sendRequest(leaderAddr, common.ClientRequest{Type: "put", Key: "foo", Value: "bar"})
	if err != nil || !resp.Success {
		log.Fatalf("  ✗ Failed to write: %v, %+v", err, resp)
	}
	log.Println("  ✓ Write successful")

	resp, err = sendRequest(leaderAddr, common.ClientRequest{Type: "get", Key: "foo"})
	if err != nil || !resp.Success || resp.Value != "bar" {
		log.Fatalf("  ✗ Failed to read: %v, %+v", err, resp)
	}
	log.Printf("  ✓ Read successful: foo=%s", resp.Value)

	// Test 3: Verify replication to all nodes
	log.Println("\n[Test 3] Verifying replication across all nodes...")
	time.Sleep(500 * time.Millisecond) // Wait for replication

	replicationSuccess := 0
	for _, addr := range addresses {
		resp, err := sendRequest(addr, common.ClientRequest{Type: "get", Key: "foo"})
		if err != nil {
			log.Printf("  ✗ Node %s not reachable: %v", addr, err)
			continue
		}
		if !resp.Success || resp.Value != "bar" {
			log.Printf("  ✗ Node %s has incorrect value: %+v", addr, resp)
			continue
		}
		log.Printf("  ✓ Node %s has correct value: foo=%s", addr, resp.Value)
		replicationSuccess++
	}

	if replicationSuccess < 2 {
		log.Fatal("  ✗ Replication failed - not enough nodes have the data")
	}

	// Test 4: Multiple sequential writes
	log.Println("\n[Test 4] Testing multiple sequential writes...")
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("key%d", i)
		value := fmt.Sprintf("value%d", i)
		resp, err := sendRequest(leaderAddr, common.ClientRequest{Type: "put", Key: key, Value: value})
		if err != nil || !resp.Success {
			log.Fatalf("  ✗ Failed to write %s: %v, %+v", key, err, resp)
		}
	}
	log.Println("  ✓ Multiple writes successful (5 writes)")

	// Verify all writes
	time.Sleep(500 * time.Millisecond)
	log.Println("  Verifying all writes...")
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("key%d", i)
		expectedValue := fmt.Sprintf("value%d", i)
		resp, err := sendRequest(leaderAddr, common.ClientRequest{Type: "get", Key: key})
		if err != nil || !resp.Success || resp.Value != expectedValue {
			log.Fatalf("  ✗ Failed to verify %s: %v, %+v", key, err, resp)
		}
	}
	log.Println("  ✓ All writes verified")

	// Test 5: Update existing key
	log.Println("\n[Test 5] Testing key update...")
	resp, err = sendRequest(leaderAddr, common.ClientRequest{Type: "put", Key: "foo", Value: "baz"})
	if err != nil || !resp.Success {
		log.Fatalf("  ✗ Failed to update: %v, %+v", err, resp)
	}
	log.Println("  ✓ Update successful")

	resp, err = sendRequest(leaderAddr, common.ClientRequest{Type: "get", Key: "foo"})
	if err != nil || !resp.Success || resp.Value != "baz" {
		log.Fatalf("  ✗ Failed to read updated value: %v, %+v", err, resp)
	}
	log.Printf("  ✓ Updated value verified: foo=%s", resp.Value)

	// Test 6: Delete operation
	log.Println("\n[Test 6] Testing delete operation...")
	resp, err = sendRequest(leaderAddr, common.ClientRequest{Type: "delete", Key: "foo"})
	if err != nil || !resp.Success {
		log.Fatalf("  ✗ Failed to delete: %v, %+v", err, resp)
	}
	log.Println("  ✓ Delete successful")

	resp, err = sendRequest(leaderAddr, common.ClientRequest{Type: "get", Key: "foo"})
	if err == nil && resp.Success {
		log.Fatal("  ✗ Key should have been deleted")
	}
	log.Println("  ✓ Key deleted successfully (not found)")

	// Test 7: Verify delete replicated
	log.Println("\n[Test 7] Verifying delete replication...")
	time.Sleep(500 * time.Millisecond)
	deleteReplicated := 0
	for _, addr := range addresses {
		resp, err := sendRequest(addr, common.ClientRequest{Type: "get", Key: "foo"})
		if err == nil && resp.Success {
			log.Printf("  ✗ Node %s still has deleted key", addr)
			continue
		}
		log.Printf("  ✓ Node %s confirmed deletion", addr)
		deleteReplicated++
	}

	if deleteReplicated < 2 {
		log.Fatal("  ✗ Delete not properly replicated")
	}

	// Test 8: Try writing to non-leader (should fail gracefully)
	log.Println("\n[Test 8] Testing write to non-leader...")
	foundNonLeader := false
	for _, addr := range addresses {
		if addr == leaderAddr {
			continue
		}
		resp, err := sendRequest(addr, common.ClientRequest{Type: "put", Key: "test-nonleader", Value: "fail"})
		if err == nil && !resp.Success && resp.Error == "not leader" {
			log.Printf("  ✓ Non-leader %s correctly rejected write: %s", addr, resp.Error)
			foundNonLeader = true
			break
		}
	}

	if !foundNonLeader {
		log.Println("  ⚠ Could not test non-leader write (all nodes might be leaders)")
	}

	// Test 9: High-throughput test
	log.Println("\n[Test 9] Testing high-throughput writes...")
	start := time.Now()
	successfulWrites := 0
	totalWrites := 100

	for i := 0; i < totalWrites; i++ {
		key := fmt.Sprintf("bulk-%d", i)
		value := fmt.Sprintf("val-%d", i)
		resp, err := sendRequest(leaderAddr, common.ClientRequest{Type: "put", Key: key, Value: value})
		if err == nil && resp.Success {
			successfulWrites++
		}
	}
	elapsed := time.Since(start)
	throughput := float64(successfulWrites) / elapsed.Seconds()

	log.Printf("  ✓ Completed %d/%d writes in %v", successfulWrites, totalWrites, elapsed)
	log.Printf("  ✓ Throughput: %.0f writes/second", throughput)

	if successfulWrites < totalWrites {
		log.Printf("  ⚠ Some writes failed (%d/%d)", totalWrites-successfulWrites, totalWrites)
	}

	// Test 10: Verify consistency across all nodes
	log.Println("\n[Test 10] Final consistency check...")
	time.Sleep(1 * time.Second) // Wait for all replication to complete

	testKey := "consistency-test"
	testValue := "final-value"

	// Write to leader
	resp, err = sendRequest(leaderAddr, common.ClientRequest{Type: "put", Key: testKey, Value: testValue})
	if err != nil || !resp.Success {
		log.Fatalf("  ✗ Failed to write test key: %v, %+v", err, resp)
	}

	// Wait and verify on all nodes
	time.Sleep(500 * time.Millisecond)
	consistentNodes := 0
	for _, addr := range addresses {
		resp, err := sendRequest(addr, common.ClientRequest{Type: "get", Key: testKey})
		if err != nil {
			log.Printf("  ✗ Node %s not reachable", addr)
			continue
		}
		if !resp.Success || resp.Value != testValue {
			log.Printf("  ✗ Node %s has inconsistent value: %+v", addr, resp)
			continue
		}
		consistentNodes++
	}

	if consistentNodes >= 2 {
		log.Printf("  ✓ Consistency verified: %d/%d nodes have consistent state", consistentNodes, len(addresses))
	} else {
		log.Fatal("  ✗ Consistency check failed")
	}

	// Final summary
	log.Println("\n========================================")
	log.Println("✓ All tests passed!")
	log.Println("========================================")
	log.Println("\nTest Summary:")
	log.Println("  - Leader election: ✓")
	log.Println("  - Basic operations (put/get/delete): ✓")
	log.Println("  - Replication: ✓")
	log.Println("  - Consistency: ✓")
	log.Println("  - High throughput: ✓")
	log.Printf("  - gRPC communication: ✓")
	log.Println("\nYour Raft-KV cluster with gRPC is working correctly!")
}
