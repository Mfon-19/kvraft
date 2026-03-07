package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"io"
	pb "kvraft/proto"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"kvraft/kvstore"
)

type ClientRequest struct {
	Type  string `json:"Type"`
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

type ClientResponse struct {
	Success bool   `json:"Success"`
	Value   string `json:"Value"`
	Error   string `json:"Error"`
}

type nodeProcess struct {
	id         int
	clientAddr string
	cmd        *exec.Cmd
	logFile    *os.File
}

func (n *nodeProcess) stop() error {
	if n == nil || n.cmd == nil || n.cmd.Process == nil {
		if n != nil && n.logFile != nil {
			return n.logFile.Close()
		}
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- n.cmd.Wait()
	}()

	_ = n.cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = n.cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	if n.logFile != nil {
		if err := n.logFile.Close(); err != nil {
			return err
		}
	}
	return nil
}

type cluster struct {
	nodes       []*nodeProcess
	clientAddrs []string
}

func (c *cluster) stopAll() {
	for _, node := range c.nodes {
		if err := node.stop(); err != nil {
			log.Printf("warning: failed stopping node %d: %v", node.id, err)
		}
	}
}

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

func (c *persistentClient) request(req ClientRequest) (ClientResponse, time.Duration, error) {
	start := time.Now()
	resp, err := invokeKV(c.client, c.timeout, req)
	if err != nil {
		return ClientResponse{}, 0, err
	}
	return resp, time.Since(start), nil
}

type raftBenchResult struct {
	Writes               int       `json:"writes"`
	LeaderAddr           string    `json:"leader_addr"`
	MeanLatencyMS        float64   `json:"mean_latency_ms"`
	P50LatencyMS         float64   `json:"p50_latency_ms"`
	P95LatencyMS         float64   `json:"p95_latency_ms"`
	P99LatencyMS         float64   `json:"p99_latency_ms"`
	ConsistencySamples   int       `json:"consistency_samples"`
	ConsistencyFailures  int       `json:"consistency_failures"`
	LatencySamplesMS     []float64 `json:"latency_samples_ms"`
	ConsistencyErrorText string    `json:"consistency_error,omitempty"`
}

type storageBenchResult struct {
	DatasetDir             string    `json:"dataset_dir"`
	DatasetKeys            int       `json:"dataset_keys"`
	DatasetRounds          int       `json:"dataset_rounds"`
	DatasetPayloadBytes    int       `json:"dataset_payload_bytes"`
	PreMergeSizeBytes      int64     `json:"pre_merge_size_bytes"`
	PostMergeSizeBytes     int64     `json:"post_merge_size_bytes"`
	DiskReductionPercent   float64   `json:"disk_reduction_percent"`
	RestartNoHintsMS       float64   `json:"restart_no_hints_ms"`
	RestartWithHintsMS     float64   `json:"restart_with_hints_ms"`
	RestartMergedHintsMS   float64   `json:"restart_merged_hints_ms"`
	RestartImprovementPct  float64   `json:"restart_improvement_percent"`
	OpenTrials             int       `json:"open_trials"`
	OpenNoHintsSamplesMS   []float64 `json:"open_no_hints_samples_ms"`
	OpenWithHintsSamplesMS []float64 `json:"open_with_hints_samples_ms"`
	OpenMergedSamplesMS    []float64 `json:"open_merged_samples_ms"`
}

type claimChecks struct {
	P99UnderTarget           bool    `json:"p99_under_target"`
	RestartUnderTarget       bool    `json:"restart_under_target"`
	DiskReductionWithinRange bool    `json:"disk_reduction_within_range"`
	P99TargetMS              float64 `json:"p99_target_ms"`
	RestartTargetMS          float64 `json:"restart_target_ms"`
	DiskReductionMinPercent  float64 `json:"disk_reduction_min_percent"`
	DiskReductionMaxPercent  float64 `json:"disk_reduction_max_percent"`
	AllTargetsMet            bool    `json:"all_targets_met"`
}

type benchmarkReport struct {
	GeneratedAtUTC string             `json:"generated_at_utc"`
	Raft           raftBenchResult    `json:"raft"`
	Storage        storageBenchResult `json:"storage"`
	Checks         claimChecks        `json:"checks"`
}

func main() {
	var (
		serverBin               = flag.String("server-bin", "./kv-server", "Path to kv-server binary")
		workDir                 = flag.String("workdir", "", "Working directory for processes and benchmark artifacts (default: temp dir)")
		keepArtifacts           = flag.Bool("keep-artifacts", false, "Keep benchmark artifacts and node logs")
		writes                  = flag.Int("writes", 600, "Number of client writes for latency measurement")
		latencyPayloadBytes     = flag.Int("latency-payload-bytes", 64, "Payload size for latency writes")
		consistencySample       = flag.Int("consistency-sample", 60, "Number of recently written keys to validate on all nodes")
		datasetKeys             = flag.Int("dataset-keys", 60000, "Number of logical keys for synthetic storage dataset")
		datasetRounds           = flag.Int("dataset-rounds", 2, "Number of overwrite rounds for synthetic dataset")
		datasetPayloadBytes     = flag.Int("dataset-payload-bytes", 512, "Value payload bytes for synthetic dataset")
		datasetMaxFileMB        = flag.Int("dataset-max-file-mb", 2, "Max data file size (MB) for synthetic dataset generation")
		restartTrials           = flag.Int("restart-trials", 5, "Number of open/close trials for restart-time metrics")
		p99TargetMS             = flag.Float64("p99-target-ms", 10.0, "Target p99 write latency in milliseconds")
		restartTargetMS         = flag.Float64("restart-target-ms", 2000.0, "Target optimized restart time in milliseconds")
		diskReductionMinPercent = flag.Float64("disk-reduction-min", 40.0, "Minimum target disk reduction percentage")
		diskReductionMaxPercent = flag.Float64("disk-reduction-max", 50.0, "Maximum target disk reduction percentage")
		strict                  = flag.Bool("strict", false, "Exit non-zero if one or more claim targets fail")
	)
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

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

	c, err := startCluster(binPath, runDir, 3)
	if err != nil {
		log.Fatalf("start cluster: %v", err)
	}

	clusterRunning := true
	defer func() {
		if clusterRunning {
			c.stopAll()
		}
	}()

	raftResult, err := runRaftBenchmark(c.clientAddrs, *writes, *latencyPayloadBytes, *consistencySample)
	if err != nil {
		log.Fatalf("raft benchmark failed: %v", err)
	}

	// Stop server processes before local storage benchmarking to keep noise and disk contention low.
	c.stopAll()
	clusterRunning = false

	storageResult, err := runStorageBenchmark(runDir, *datasetKeys, *datasetRounds, *datasetPayloadBytes, int64(*datasetMaxFileMB)*1024*1024, *restartTrials)
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
	fmt.Printf("  artifacts dir: %s\n", runDir)

	if *strict && !checks.AllTargetsMet {
		os.Exit(1)
	}
}

func startCluster(serverBin, runDir string, n int) (*cluster, error) {
	raftPorts, err := reserveFreePorts(n)
	if err != nil {
		return nil, err
	}
	clientPorts, err := reserveFreePorts(n)
	if err != nil {
		return nil, err
	}

	raftAddrs := make([]string, 0, n)
	for _, p := range raftPorts {
		raftAddrs = append(raftAddrs, "127.0.0.1:"+strconv.Itoa(p))
	}
	clientAddrs := make([]string, 0, n)
	for _, p := range clientPorts {
		clientAddrs = append(clientAddrs, "127.0.0.1:"+strconv.Itoa(p))
	}

	peersFlag := strings.Join(raftAddrs, ",")

	nodes := make([]*nodeProcess, 0, n)
	for i := 0; i < n; i++ {
		logPath := filepath.Join(runDir, fmt.Sprintf("node%d.log", i))
		lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			for _, node := range nodes {
				_ = node.stop()
			}
			return nil, fmt.Errorf("open log file for node %d: %w", i, err)
		}

		cmd := exec.Command(
			serverBin,
			"-id="+strconv.Itoa(i),
			"-port="+strconv.Itoa(raftPorts[i]),
			"-client-port="+strconv.Itoa(clientPorts[i]),
			"-peers="+peersFlag,
		)
		cmd.Dir = runDir
		cmd.Stdout = lf
		cmd.Stderr = lf

		if err := cmd.Start(); err != nil {
			_ = lf.Close()
			for _, node := range nodes {
				_ = node.stop()
			}
			return nil, fmt.Errorf("start node %d: %w", i, err)
		}

		nodes = append(nodes, &nodeProcess{id: i, clientAddr: clientAddrs[i], cmd: cmd, logFile: lf})
	}

	return &cluster{nodes: nodes, clientAddrs: clientAddrs}, nil
}

func reserveFreePorts(n int) ([]int, error) {
	ports := make([]int, 0, n)
	listeners := make([]net.Listener, 0, n)
	seen := make(map[int]struct{})

	for len(ports) < n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return nil, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if _, ok := seen[port]; ok {
			_ = ln.Close()
			continue
		}
		seen[port] = struct{}{}
		listeners = append(listeners, ln)
		ports = append(ports, port)
	}

	for _, ln := range listeners {
		_ = ln.Close()
	}
	return ports, nil
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
	// Reuse one write connection so latency reflects steady-state service time instead of per-request dial cost.
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

		resp, _, err := sendRequest(leader, ClientRequest{Type: "get", Key: key}, 1500*time.Millisecond)
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
		if err := waitForReplication(addresses, key, expected[key], 4*time.Second); err != nil {
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
		result.ConsistencyErrorText = fmt.Sprintf("%d keys did not converge to the expected value on all nodes", failureCount)
	}
	return result, nil
}

func discoverLeader(addresses []string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	probeValue := fmt.Sprintf("%d", time.Now().UnixNano())

	for time.Now().Before(deadline) {
		for _, addr := range addresses {
			resp, _, err := sendRequest(addr, ClientRequest{Type: "put", Key: "__bench_leader_probe__", Value: probeValue}, 1500*time.Millisecond)
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

		resp, d, err := (*client).request(ClientRequest{Type: "put", Key: key, Value: value})
		if err == nil && resp.Success {
			return d, *leader, nil
		}

		// Leadership can move while the benchmark is running; clear and retry after rediscovery.
		if err != nil || strings.Contains(strings.ToLower(resp.Error), "not leader") {
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

func waitForReplication(addresses []string, key, expected string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		allMatch := true
		for _, addr := range addresses {
			resp, _, err := sendRequest(addr, ClientRequest{Type: "get", Key: key}, 1200*time.Millisecond)
			if err != nil || !resp.Success || resp.Value != expected {
				allMatch = false
				break
			}
		}
		if allMatch {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("replication timeout for key %s", key)
}

func sendRequest(address string, req ClientRequest, timeout time.Duration) (ClientResponse, time.Duration, error) {
	start := time.Now()
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return ClientResponse{}, 0, err
	}
	defer conn.Close()

	client := pb.NewKVServiceClient(conn)
	resp, err := invokeKV(client, timeout, req)
	if err != nil {
		return ClientResponse{}, 0, err
	}
	return resp, time.Since(start), nil
}

func invokeKV(client pb.KVServiceClient, timeout time.Duration, req ClientRequest) (ClientResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	switch req.Type {
	case "get":
		resp, err := client.Get(ctx, &pb.KVRequest{Key: req.Key})
		if err != nil {
			return ClientResponse{}, err
		}
		return ClientResponse{Success: resp.Success, Value: resp.Value, Error: resp.Error}, nil
	case "put":
		resp, err := client.Put(ctx, &pb.KVRequest{Key: req.Key, Value: req.Value})
		if err != nil {
			return ClientResponse{}, err
		}
		return ClientResponse{Success: resp.Success, Value: resp.Value, Error: resp.Error}, nil
	case "delete":
		resp, err := client.Delete(ctx, &pb.KVRequest{Key: req.Key})
		if err != nil {
			return ClientResponse{}, err
		}
		return ClientResponse{Success: resp.Success, Value: resp.Value, Error: resp.Error}, nil
	default:
		return ClientResponse{Success: false, Error: "unknown command"}, nil
	}
}

func runStorageBenchmark(runDir string, keys, rounds, payloadBytes int, maxDataFileSizeBytes int64, trials int) (storageBenchResult, error) {
	if keys <= 0 || rounds <= 0 || payloadBytes <= 0 {
		return storageBenchResult{}, fmt.Errorf("dataset keys, rounds, and payload bytes must all be > 0")
	}
	if maxDataFileSizeBytes <= 0 {
		return storageBenchResult{}, fmt.Errorf("dataset max data file size must be > 0")
	}
	if trials <= 0 {
		return storageBenchResult{}, fmt.Errorf("restart trials must be > 0")
	}

	datasetSrc := filepath.Join(runDir, "storage_dataset_src")
	if err := os.RemoveAll(datasetSrc); err != nil {
		return storageBenchResult{}, err
	}
	if err := buildSyntheticDataset(datasetSrc, keys, rounds, payloadBytes, maxDataFileSizeBytes); err != nil {
		return storageBenchResult{}, err
	}

	withHints := filepath.Join(runDir, "storage_with_hints")
	noHints := filepath.Join(runDir, "storage_no_hints")
	merged := filepath.Join(runDir, "storage_merged")

	// Clone the same dataset for baseline and optimized paths so the comparison is apples-to-apples.
	if err := copyDir(datasetSrc, withHints); err != nil {
		return storageBenchResult{}, err
	}
	if err := copyDir(datasetSrc, noHints); err != nil {
		return storageBenchResult{}, err
	}
	if err := copyDir(datasetSrc, merged); err != nil {
		return storageBenchResult{}, err
	}

	if err := removeHintFiles(noHints); err != nil {
		return storageBenchResult{}, err
	}

	preMergeSize, err := dirSizeBytes(merged)
	if err != nil {
		return storageBenchResult{}, err
	}

	if err := mergeDirectory(merged); err != nil {
		return storageBenchResult{}, err
	}

	postMergeSize, err := dirSizeBytes(merged)
	if err != nil {
		return storageBenchResult{}, err
	}

	noHintsSamples, err := measureOpenSamples(noHints, trials)
	if err != nil {
		return storageBenchResult{}, err
	}
	withHintsSamples, err := measureOpenSamples(withHints, trials)
	if err != nil {
		return storageBenchResult{}, err
	}
	mergedSamples, err := measureOpenSamples(merged, trials)
	if err != nil {
		return storageBenchResult{}, err
	}

	sortedNoHints := append([]float64(nil), noHintsSamples...)
	sortedWithHints := append([]float64(nil), withHintsSamples...)
	sortedMerged := append([]float64(nil), mergedSamples...)
	sort.Float64s(sortedNoHints)
	sort.Float64s(sortedWithHints)
	sort.Float64s(sortedMerged)

	restartNoHints := percentile(sortedNoHints, 0.50)
	restartWithHints := percentile(sortedWithHints, 0.50)
	restartMerged := percentile(sortedMerged, 0.50)

	diskReductionPct := 0.0
	if preMergeSize > 0 {
		diskReductionPct = (float64(preMergeSize-postMergeSize) / float64(preMergeSize)) * 100
	}

	restartImprovement := 0.0
	if restartNoHints > 0 {
		restartImprovement = (restartNoHints - restartMerged) / restartNoHints * 100
	}

	return storageBenchResult{
		DatasetDir:             datasetSrc,
		DatasetKeys:            keys,
		DatasetRounds:          rounds,
		DatasetPayloadBytes:    payloadBytes,
		PreMergeSizeBytes:      preMergeSize,
		PostMergeSizeBytes:     postMergeSize,
		DiskReductionPercent:   diskReductionPct,
		RestartNoHintsMS:       restartNoHints,
		RestartWithHintsMS:     restartWithHints,
		RestartMergedHintsMS:   restartMerged,
		RestartImprovementPct:  restartImprovement,
		OpenTrials:             trials,
		OpenNoHintsSamplesMS:   sortedNoHints,
		OpenWithHintsSamplesMS: sortedWithHints,
		OpenMergedSamplesMS:    sortedMerged,
	}, nil
}

func buildSyntheticDataset(dir string, keys, rounds, payloadBytes int, maxDataFileSizeBytes int64) error {
	db, err := kvstore.OpenWithOptions(dir, kvstore.OpenOptions{
		ReadWrite:            true,
		SyncOnPut:            false,
		MaxDataFileSizeBytes: maxDataFileSizeBytes,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	payload := strings.Repeat("v", payloadBytes)
	for r := 0; r < rounds; r++ {
		for k := 0; k < keys; k++ {
			key := fmt.Sprintf("dataset-%08d", k)
			value := fmt.Sprintf("r%02d:%s", r, payload)
			if err := db.Put(key, value); err != nil {
				return err
			}
		}
	}

	return db.Sync()
}

func copyDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}

	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}

		if _, err := io.Copy(out, in); err != nil {
			_ = in.Close()
			_ = out.Close()
			return err
		}
		if err := in.Close(); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}

func removeHintFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".hint") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergeDirectory(dir string) error {
	db, err := kvstore.OpenWithOptions(dir, kvstore.OpenOptions{ReadWrite: true})
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Merge(); err != nil {
		return err
	}
	return db.Sync()
}

func dirSizeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func measureOpenSamples(dir string, trials int) ([]float64, error) {
	samples := make([]float64, 0, trials)
	for i := 0; i < trials; i++ {
		start := time.Now()
		db, err := kvstore.OpenReadOnly(dir)
		if err != nil {
			return nil, err
		}
		if err := db.Close(); err != nil {
			return nil, err
		}
		samples = append(samples, float64(time.Since(start).Microseconds())/1000.0)
	}
	return samples, nil
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

	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
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
