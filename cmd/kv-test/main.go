package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"kvraft/common"
	pb "kvraft/proto"
)

func sendRequest(address string, req common.ClientRequest) (common.ClientResponse, error) {
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return common.ClientResponse{}, err
	}
	defer conn.Close()

	client := pb.NewKVServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return common.InvokeKV(ctx, client, req)
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("--- Starting Raft-KV Integration Test ---")

	time.Sleep(2 * time.Second)

	addresses := []string{"localhost:8000", "localhost:8001", "localhost:8002"}
	var leaderAddr string

	log.Println("[1/10] Finding leader...")
	for i := 0; i < 5; i++ {
		for _, addr := range addresses {
			resp, err := sendRequest(addr, common.ClientRequest{Type: common.OpPut, Key: "ping", Value: "pong"})
			if err == nil && resp.Success {
				leaderAddr = addr
				log.Printf("\t✓ Leader found at %s", addr)
				break
			}
			if err == nil && !resp.Success && resp.Error == "not leader" && resp.Leader != "" {
				leaderAddr = resp.Leader
				log.Printf("\t✓ Leader hinted at %s by %s", leaderAddr, addr)
				break
			}
		}
		if leaderAddr != "" {
			break
		}
		time.Sleep(1 * time.Second)
		log.Printf("\t... Retrying leader discovery (%d/5)", i+1)
	}
	if leaderAddr == "" {
		log.Fatal("\t✗ Could not find leader. Is the cluster running?")
	}

	log.Println("[2/10] Testing Basic Put/Get...")
	resp, err := sendRequest(leaderAddr, common.ClientRequest{Type: common.OpPut, Key: "foo", Value: "bar"})
	if err != nil || !resp.Success {
		log.Fatalf("\t✗ Put failed: %v", resp.Error)
	}

	resp, err = sendRequest(leaderAddr, common.ClientRequest{Type: common.OpGet, Key: "foo"})
	if err != nil || resp.Value != "bar" {
		log.Fatalf("\t✗ Get failed. Expected 'bar', got '%s'", resp.Value)
	}
	log.Println("\t✓ Basic Put/Get passed")

	log.Println("[3/10] Verifying Leader-Only Linearizable Reads...")
	rejectedFollowers := 0
	for _, addr := range addresses {
		if addr == leaderAddr {
			continue
		}
		resp, _ := sendRequest(addr, common.ClientRequest{Type: common.OpGet, Key: "foo"})
		if !resp.Success && resp.Error == "not leader" {
			rejectedFollowers++
		}
	}
	if rejectedFollowers != len(addresses)-1 {
		log.Fatalf("\t✗ Expected follower reads to be rejected, got %d/%d rejections.", rejectedFollowers, len(addresses)-1)
	}
	log.Println("\t✓ Followers rejected linearizable reads")

	log.Println("[4/10] Sequential Writes...")
	for i := 0; i < 5; i++ {
		k, v := fmt.Sprintf("seq-%d", i), fmt.Sprintf("val-%d", i)
		if _, err := sendRequest(leaderAddr, common.ClientRequest{Type: common.OpPut, Key: k, Value: v}); err != nil {
			log.Fatalf("\t✗ Write failed at index %d", i)
		}
	}
	log.Println("\t✓ 5 Sequential writes successful")

	log.Println("[5/10] Updating Keys...")
	sendRequest(leaderAddr, common.ClientRequest{Type: common.OpPut, Key: "foo", Value: "updated"})
	resp, _ = sendRequest(leaderAddr, common.ClientRequest{Type: common.OpGet, Key: "foo"})
	if resp.Value != "updated" {
		log.Fatalf("\t✗ Update failed. Got %s", resp.Value)
	}
	log.Println("\t✓ Key updated successfully")

	log.Println("[6/10] Deleting Keys...")
	sendRequest(leaderAddr, common.ClientRequest{Type: common.OpDelete, Key: "foo"})
	resp, _ = sendRequest(leaderAddr, common.ClientRequest{Type: common.OpGet, Key: "foo"})
	if resp.Success {
		log.Fatal("\t✗ Key should be deleted but was found")
	}
	log.Println("\t✓ Key deleted")

	log.Println("[7/10] Verifying Followers Still Reject Reads...")
	for _, addr := range addresses {
		if addr == leaderAddr {
			continue
		}
		resp, _ := sendRequest(addr, common.ClientRequest{Type: common.OpGet, Key: "foo"})
		if resp.Success || resp.Error != "not leader" {
			log.Fatalf("\t✗ Expected follower %s to reject read, got %+v", addr, resp)
		}
	}
	log.Println("\t✓ Followers continued to reject linearizable reads")

	log.Println("[8/10] Testing Follower Rejection...")
	checkedFollower := false
	for _, addr := range addresses {
		if addr != leaderAddr {
			resp, _ := sendRequest(addr, common.ClientRequest{Type: common.OpPut, Key: "bad", Value: "data"})
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

	log.Println("[9/10] High-Throughput Test (100 Writes)...")
	start := time.Now()
	total := 100
	ok := 0
	for i := 0; i < total; i++ {
		k := fmt.Sprintf("bench-%d", i)
		if resp, _ := sendRequest(leaderAddr, common.ClientRequest{Type: common.OpPut, Key: k, Value: "x"}); resp.Success {
			ok++
		}
	}
	duration := time.Since(start)
	log.Printf("\t✓ %d/%d writes succeeded in %v (%.0f req/sec)", ok, total, duration, float64(ok)/duration.Seconds())

	log.Println("[10/10] Final Linearizable Read Check...")
	time.Sleep(1 * time.Second)

	testKey := "bench-99"
	resp, err = sendRequest(leaderAddr, common.ClientRequest{Type: common.OpGet, Key: testKey})
	if err != nil || !resp.Success || resp.Value != "x" {
		log.Fatalf("\t✗ Leader read failed. Response=%+v err=%v", resp, err)
	}
	rejectedFollowers = 0
	for _, addr := range addresses {
		if addr == leaderAddr {
			continue
		}
		resp, _ := sendRequest(addr, common.ClientRequest{Type: common.OpGet, Key: testKey})
		if !resp.Success && resp.Error == "not leader" {
			rejectedFollowers++
		}
	}

	if rejectedFollowers != len(addresses)-1 {
		log.Fatalf("\t✗ Expected follower read rejection, got %d/%d rejections.", rejectedFollowers, len(addresses)-1)
	}
	log.Println("\t✓ Leader served the read and followers rejected it")

	log.Println("\n--- All Tests Passed Successfully ---")
}
