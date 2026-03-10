package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"kvraft/common"
	benchcluster "kvraft/internal/bench/cluster"
	benchcompare "kvraft/internal/bench/compare"
	benchreport "kvraft/internal/bench/report"
	benchstorage "kvraft/internal/bench/storage"
	pb "kvraft/proto"
)

type persistentClient struct {
	address string
	timeout time.Duration
	conn    *grpc.ClientConn
	client  pb.KVServiceClient
}

func newPersistentClient(address string, timeout time.Duration) (*persistentClient, error) {
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &persistentClient{
		address: address,
		timeout: timeout,
		conn:    conn,
		client:  pb.NewKVServiceClient(conn),
	}, nil
}

func (c *persistentClient) close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *persistentClient) request(req common.ClientRequest) (common.ClientResponse, time.Duration, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	resp, err := common.InvokeKV(ctx, c.client, req)
	cancel()
	if err != nil {
		return common.ClientResponse{}, 0, err
	}
	return resp, time.Since(start), nil
}

type raftBenchResult = benchreport.RaftBenchResult
type storageBenchResult = benchreport.StorageBenchResult
type claimChecks = benchreport.ClaimChecks
type benchmarkReport = benchreport.BenchmarkReport

func main() {
	var (
		serverBin               = flag.String("server-bin", "./kv-server", "Path to kv-server binary")
		workDir                 = flag.String("workdir", "", "Working directory for processes and benchmark artifacts (default: temp dir)")
		keepArtifacts           = flag.Bool("keep-artifacts", false, "Keep benchmark artifacts and node logs")
		writes                  = flag.Int("writes", 600, "Number of client writes for latency measurement")
		latencyPayloadBytes     = flag.Int("latency-payload-bytes", 64, "Payload size for latency writes")
		consistencySample       = flag.Int("consistency-sample", 60, "Number of recently written keys to validate through leader reads")
		datasetKeys             = flag.Int("dataset-keys", 60000, "Number of logical keys for synthetic storage dataset")
		datasetRounds           = flag.Int("dataset-rounds", 2, "Number of overwrite rounds for synthetic dataset")
		datasetPayloadBytes     = flag.Int("dataset-payload-bytes", 512, "Value payload bytes for synthetic dataset")
		datasetMaxFileMB        = flag.Int("dataset-max-file-mb", 2, "Max data file size (MB) for synthetic dataset generation")
		restartTrials           = flag.Int("restart-trials", 5, "Number of open/close trials for restart-time metrics")
		p99TargetMS             = flag.Float64("p99-target-ms", 10.0, "Target p99 write latency in milliseconds")
		restartTargetMS         = flag.Float64("restart-target-ms", 2000.0, "Target optimized restart time in milliseconds")
		diskReductionMinPercent = flag.Float64("disk-reduction-min", 40.0, "Minimum target disk reduction percentage")
		diskReductionMaxPercent = flag.Float64("disk-reduction-max", 50.0, "Maximum target disk reduction percentage")
		baselinePath            = flag.String("baseline", "", "Optional baseline JSON path for no-regression comparison")
		strict                  = flag.Bool("strict", false, "Exit non-zero if one or more claim targets fail")
	)
	flag.Parse()

	binPath, err := filepath.Abs(*serverBin)
	if err != nil {
		log.Fatalf("resolve server binary: %v", err)
	}
	if _, err := os.Stat(binPath); err != nil {
		log.Fatalf("server binary not found at %s: %v", binPath, err)
	}

	runDir := *workDir
	if runDir == "" {
		runDir, err = os.MkdirTemp("", "kvraft-bench-")
		if err != nil {
			log.Fatalf("create temp workdir: %v", err)
		}
	} else {
		runDir, err = filepath.Abs(runDir)
		if err != nil {
			log.Fatalf("resolve workdir: %v", err)
		}
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			log.Fatalf("create workdir: %v", err)
		}
	}

	if !*keepArtifacts {
		defer func() {
			_ = os.RemoveAll(runDir)
		}()
	}

	log.Printf("benchmark workdir: %s", runDir)

	c, err := benchcluster.Start(binPath, runDir, 3)
	if err != nil {
		log.Fatalf("start cluster: %v", err)
	}

	clusterRunning := true
	defer func() {
		if clusterRunning {
			c.StopAll()
		}
	}()

	raftResult, err := runRaftBenchmark(c.ClientAddrs, *writes, *latencyPayloadBytes, *consistencySample)
	if err != nil {
		log.Fatalf("raft benchmark failed: %v", err)
	}

	// Stop server processes before local storage benchmarking to keep noise and disk contention low.
	c.StopAll()
	clusterRunning = false

	storageResult, err := benchstorage.Run(runDir, *datasetKeys, *datasetRounds, *datasetPayloadBytes, int64(*datasetMaxFileMB)*1024*1024, *restartTrials)
	if err != nil {
		log.Fatalf("storage benchmark failed: %v", err)
	}

	checks := claimChecks{
		P99UnderTarget:           raftResult.P99LatencyMS < *p99TargetMS,
		RestartUnderTarget:       storageResult.RestartMergedHintsMS < *restartTargetMS,
		DiskReductionWithinRange: storageResult.DiskReductionPercent >= *diskReductionMinPercent && storageResult.DiskReductionPercent <= *diskReductionMaxPercent,
		P99TargetMS:              *p99TargetMS,
		RestartTargetMS:          *restartTargetMS,
		DiskReductionMinPercent:  *diskReductionMinPercent,
		DiskReductionMaxPercent:  *diskReductionMaxPercent,
	}
	checks.AllTargetsMet = checks.P99UnderTarget && checks.RestartUnderTarget && checks.DiskReductionWithinRange

	report := benchmarkReport{
		GeneratedAtUTC: time.Now().UTC().Format(time.RFC3339),
		Raft:           raftResult,
		Storage:        storageResult,
		Checks:         checks,
	}

	var baselineResult *benchcompare.Result
	if *baselinePath != "" {
		base, err := benchcompare.LoadBaseline(*baselinePath)
		if err != nil {
			log.Fatalf("load baseline: %v", err)
		}
		res := benchcompare.Compare(report, *baselinePath, base)
		baselineResult = &res
	}

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatalf("marshal report: %v", err)
	}

	fmt.Println(string(encoded))
	fmt.Printf("\nSummary:\n")
	fmt.Printf("  p99 write latency: %.2fms (target < %.2fms)\n", raftResult.P99LatencyMS, *p99TargetMS)
	fmt.Printf("  optimized restart: %.2fms (target < %.2fms)\n", storageResult.RestartMergedHintsMS, *restartTargetMS)
	fmt.Printf("  disk reduction: %.2f%% (target %.2f%%-%.2f%%)\n", storageResult.DiskReductionPercent, *diskReductionMinPercent, *diskReductionMaxPercent)
	fmt.Printf("  all targets met: %v\n", checks.AllTargetsMet)
	if baselineResult != nil {
		fmt.Printf("  baseline comparison (%s): %v\n", baselineResult.BaselinePath, baselineResult.AllPass)
		for _, c := range baselineResult.Checks {
			fmt.Printf("    - %s: %.3f %s %.3f => %v\n", c.Name, c.Actual, c.Comparator, c.Threshold, c.Pass)
		}
	}
	fmt.Printf("  artifacts dir: %s\n", runDir)

	if *strict {
		allTargets := checks.AllTargetsMet
		if baselineResult != nil {
			allTargets = allTargets && baselineResult.AllPass
		}
		if !allTargets {
			os.Exit(1)
		}
	}
}

func runRaftBenchmark(addresses []string, writes, payloadBytes, consistencySample int) (raftBenchResult, error) {
	if writes <= 0 {
		return raftBenchResult{}, fmt.Errorf("writes must be > 0")
	}
	if payloadBytes <= 0 {
		return raftBenchResult{}, fmt.Errorf("latency payload bytes must be > 0")
	}

	leader, err := discoverLeader(addresses, 20*time.Second)
	if err != nil {
		return raftBenchResult{}, err
	}

	writeClient, err := newPersistentClient(leader, 3*time.Second)
	if err != nil {
		return raftBenchResult{}, fmt.Errorf("open persistent write connection: %w", err)
	}
	defer writeClient.close()

	expected := make(map[string]string, writes)
	keys := make([]string, 0, writes)
	latenciesMS := make([]float64, 0, writes)
	payload := strings.Repeat("x", payloadBytes)

	for i := 0; i < writes; i++ {
		key := fmt.Sprintf("bench-lat-%08d", i)
		value := fmt.Sprintf("%08d:%s", i, payload)

		latency, leaderOut, err := putWithRetryPersistent(addresses, &leader, &writeClient, key, value)
		if err != nil {
			return raftBenchResult{}, fmt.Errorf("write %d failed: %w", i, err)
		}
		leader = leaderOut
		latenciesMS = append(latenciesMS, float64(latency.Microseconds())/1000.0)

		resp, _, err := sendRequest(leader, common.ClientRequest{Type: common.OpGet, Key: key}, 1500*time.Millisecond)
		if err != nil {
			return raftBenchResult{}, fmt.Errorf("leader read-after-write failed for key %s: %w", key, err)
		}
		if !resp.Success || resp.Value != value {
			return raftBenchResult{}, fmt.Errorf("leader read-after-write mismatch for key %s", key)
		}

		expected[key] = value
		keys = append(keys, key)
	}

	failureCount := 0
	samples := consistencySample
	if samples > len(keys) {
		samples = len(keys)
	}
	for _, key := range keys[len(keys)-samples:] {
		if _, err := waitForCommittedRead(addresses, leader, key, expected[key], 4*time.Second); err != nil {
			failureCount++
		}
	}

	sorted := append([]float64(nil), latenciesMS...)
	sort.Float64s(sorted)
	result := raftBenchResult{
		Writes:              writes,
		LeaderAddr:          leader,
		MeanLatencyMS:       mean(latenciesMS),
		P50LatencyMS:        percentile(sorted, 0.50),
		P95LatencyMS:        percentile(sorted, 0.95),
		P99LatencyMS:        percentile(sorted, 0.99),
		ConsistencySamples:  samples,
		ConsistencyFailures: failureCount,
		LatencySamplesMS:    sorted,
	}
	if failureCount > 0 {
		result.ConsistencyErrorText = fmt.Sprintf("%d keys were not readable with the expected value from the leader within the timeout", failureCount)
	}
	return result, nil
}

func discoverLeader(addresses []string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	probeValue := fmt.Sprintf("%d", time.Now().UnixNano())

	for time.Now().Before(deadline) {
		for _, addr := range addresses {
			resp, _, err := sendRequest(addr, common.ClientRequest{Type: common.OpPut, Key: "__bench_leader_probe__", Value: probeValue}, 1500*time.Millisecond)
			if err != nil {
				continue
			}
			if resp.Success {
				return addr, nil
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	return "", fmt.Errorf("leader discovery timed out")
}

func putWithRetryPersistent(addresses []string, leader *string, client **persistentClient, key, value string) (time.Duration, string, error) {
	const maxAttempts = 12

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if *leader == "" {
			discovered, err := discoverLeader(addresses, 3*time.Second)
			if err != nil {
				time.Sleep(150 * time.Millisecond)
				continue
			}
			*leader = discovered
		}

		if *client == nil || (*client).address != *leader {
			if *client != nil {
				_ = (*client).close()
				*client = nil
			}
			newClient, err := newPersistentClient(*leader, 3*time.Second)
			if err != nil {
				*leader = ""
				time.Sleep(100 * time.Millisecond)
				continue
			}
			*client = newClient
		}

		resp, d, err := (*client).request(common.ClientRequest{Type: common.OpPut, Key: key, Value: value})
		if err == nil && resp.Success {
			return d, *leader, nil
		}

		if err != nil || isNotLeader(resp.Error) {
			if *client != nil {
				_ = (*client).close()
				*client = nil
			}
			*leader = ""
			time.Sleep(100 * time.Millisecond)
			continue
		}

		return 0, "", fmt.Errorf("put rejected by %s: %s", *leader, resp.Error)
	}

	return 0, "", fmt.Errorf("put failed after retries")
}

func waitForCommittedRead(addresses []string, leaderHint, key, expected string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	leader := leaderHint

	for time.Now().Before(deadline) {
		if leader == "" {
			discovered, err := discoverLeader(addresses, 2*time.Second)
			if err != nil {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			leader = discovered
		}

		resp, _, err := sendRequest(leader, common.ClientRequest{Type: common.OpGet, Key: key}, 1200*time.Millisecond)
		if err == nil && resp.Success && resp.Value == expected {
			return leader, nil
		}
		if err != nil || isNotLeader(resp.Error) {
			leader = ""
		}
		time.Sleep(50 * time.Millisecond)
	}

	return leader, fmt.Errorf("leader visibility timeout for key %s", key)
}

func sendRequest(address string, req common.ClientRequest, timeout time.Duration) (common.ClientResponse, time.Duration, error) {
	start := time.Now()
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return common.ClientResponse{}, 0, err
	}
	defer conn.Close()

	client := pb.NewKVServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	resp, err := common.InvokeKV(ctx, client, req)
	cancel()
	if err != nil {
		return common.ClientResponse{}, 0, err
	}
	return resp, time.Since(start), nil
}

func isNotLeader(errText string) bool {
	return strings.Contains(strings.ToLower(errText), "not leader")
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(float64(len(sorted)) * q)
	if float64(idx) < float64(len(sorted))*q {
		idx++
	}
	idx--
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}
