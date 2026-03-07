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
	"sync"
	"sync/atomic"
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
	GeneratedAtUTC string                `json:"generated_at_utc"`
	Raft           raftBenchResult       `json:"raft"`
	Storage        storageBenchResult    `json:"storage"`
	EtcdComparison *etcdComparisonResult `json:"etcd_comparison,omitempty"`
	Checks         claimChecks           `json:"checks"`
}

type latencySummary struct {
	Samples      int       `json:"samples"`
	MeanLatency  float64   `json:"mean_latency_ms"`
	P50Latency   float64   `json:"p50_latency_ms"`
	P95Latency   float64   `json:"p95_latency_ms"`
	P99Latency   float64   `json:"p99_latency_ms"`
	MaxLatency   float64   `json:"max_latency_ms"`
	StdDev       float64   `json:"stddev_latency_ms"`
	LatencyTrace []float64 `json:"latency_samples_ms,omitempty"`
}

type loadScenarioResult struct {
	Name             string    `json:"name"`
	Workers          int       `json:"workers"`
	DurationSeconds  float64   `json:"duration_seconds"`
	Requests         int       `json:"requests"`
	Successes        int       `json:"successes"`
	Failures         int       `json:"failures"`
	ThroughputRPS    float64   `json:"throughput_rps"`
	MeanLatencyMS    float64   `json:"mean_latency_ms"`
	P50LatencyMS     float64   `json:"p50_latency_ms"`
	P95LatencyMS     float64   `json:"p95_latency_ms"`
	P99LatencyMS     float64   `json:"p99_latency_ms"`
	SlowestLatencyMS float64   `json:"slowest_latency_ms"`
	StdDevLatencyMS  float64   `json:"stddev_latency_ms"`
	LatencySamplesMS []float64 `json:"latency_samples_ms,omitempty"`
}

type diskLatencyResult struct {
	WALFsync                 latencySummary `json:"wal_fsync"`
	BackendCommit            latencySummary `json:"backend_commit"`
	WALP99TargetMS           float64        `json:"wal_p99_target_ms"`
	BackendCommitTargetMS    float64        `json:"backend_commit_target_ms"`
	WALP99UnderTarget        bool           `json:"wal_p99_under_target"`
	BackendCommitUnderTarget bool           `json:"backend_commit_under_target"`
}

type checkPerfModelResult struct {
	Model              string             `json:"model"`
	MinThroughputRPS   float64            `json:"min_throughput_rps"`
	SlowestTargetMS    float64            `json:"slowest_target_ms"`
	StdDevTargetMS     float64            `json:"stddev_target_ms"`
	Result             loadScenarioResult `json:"result"`
	ThroughputPass     bool               `json:"throughput_pass"`
	SlowestLatencyPass bool               `json:"slowest_latency_pass"`
	LatencyStdDevPass  bool               `json:"latency_stddev_pass"`
	AllPass            bool               `json:"all_pass"`
}

type etcdTargetCheck struct {
	Name       string  `json:"name"`
	Actual     float64 `json:"actual"`
	Target     float64 `json:"target"`
	Comparator string  `json:"comparator"`
	Pass       bool    `json:"pass"`
}

type etcdComparisonResult struct {
	KeyBytes         int                    `json:"key_bytes"`
	ValueBytes       int                    `json:"value_bytes"`
	ReadKeyspace     int                    `json:"read_keyspace"`
	WorkloadSeconds  int                    `json:"workload_seconds"`
	WriteLeader      loadScenarioResult     `json:"write_leader_targeted"`
	WriteAllMembers  loadScenarioResult     `json:"write_all_members_targeted"`
	ReadLinearizable loadScenarioResult     `json:"read_linearizable"`
	ReadSerializable loadScenarioResult     `json:"read_serializable"`
	LightLoadPut     latencySummary         `json:"light_load_put"`
	LightLoadGet     latencySummary         `json:"light_load_get"`
	DiskLatency      diskLatencyResult      `json:"disk_latency"`
	CheckPerf        []checkPerfModelResult `json:"check_perf"`
	TargetChecks     []etcdTargetCheck      `json:"target_checks"`
	AllTargetsMet    bool                   `json:"all_targets_met"`
}

type etcdComparisonConfig struct {
	WorkloadDuration   time.Duration
	WriteWorkers       int
	ReadWorkers        int
	PayloadBytes       int
	ReadKeyspace       int
	LightLoadOps       int
	DiskFsyncSamples   int
	DiskCommitSamples  int
	DiskCommitBatch    int
	CheckPerfDuration  time.Duration
	CheckPerfKeepTrace bool
}

const (
	etcdTargetWriteLeaderRPS     = 44000.0
	etcdTargetWriteLeaderMeanMS  = 22.0
	etcdTargetWriteAllRPS        = 50000.0
	etcdTargetWriteAllMeanMS     = 20.0
	etcdTargetReadLinearRPS      = 141000.0
	etcdTargetReadLinearMeanMS   = 5.5
	etcdTargetReadSerialRPS      = 186000.0
	etcdTargetReadSerialMeanMS   = 2.2
	etcdTargetLightLatencyMS     = 1.0
	etcdTargetWALFsyncP99MS      = 10.0
	etcdTargetBackendCommitP99MS = 25.0
	etcdCheckPerfSlowestMaxMS    = 500.0
	etcdCheckPerfStdDevMaxMS     = 100.0
	etcdLoadRequestTimeout       = 4 * time.Second
)

type loadMode int

const (
	loadModeWriteLeader loadMode = iota
	loadModeWriteAllMembers
	loadModeReadLeader
	loadModeReadAllMembers
)

type loadScenarioConfig struct {
	Name         string
	Mode         loadMode
	Addresses    []string
	Duration     time.Duration
	Workers      int
	PayloadBytes int
	ReadKeys     []string
	LeaderHint   string
	KeyStart     uint64
	KeepTrace    bool
}

type workerMetrics struct {
	successes int
	failures  int
	latencies []float64
}

type checkPerfModelConfig struct {
	Name             string
	MinThroughputRPS float64
	Workers          int
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
		etcdCompare             = flag.Bool("etcd-compare", false, "Run etcd-style throughput/latency comparison suite")
		etcdDurationSec         = flag.Int("etcd-duration-sec", 20, "Duration in seconds for heavy-load write/read scenarios")
		etcdWriteWorkers        = flag.Int("etcd-write-workers", 64, "Worker count for heavy-load write scenarios")
		etcdReadWorkers         = flag.Int("etcd-read-workers", 96, "Worker count for heavy-load read scenarios")
		etcdPayloadBytes        = flag.Int("etcd-payload-bytes", 256, "Value payload bytes for etcd comparison workloads")
		etcdReadKeyspace        = flag.Int("etcd-read-keyspace", 5000, "Number of keys preloaded before read-heavy scenarios")
		etcdLightOps            = flag.Int("etcd-light-ops", 200, "Number of sequential operations for light-load latency sampling")
		etcdDiskFsyncSamples    = flag.Int("etcd-disk-fsync-samples", 400, "Number of SyncOnPut samples for WAL fsync proxy")
		etcdDiskCommitSamples   = flag.Int("etcd-disk-commit-samples", 200, "Number of explicit Sync samples for backend commit proxy")
		etcdDiskCommitBatch     = flag.Int("etcd-disk-commit-batch", 32, "Number of puts between backend commit Sync samples")
		etcdCheckPerfSec        = flag.Int("etcd-check-perf-sec", 10, "Duration in seconds for each etcdctl check-perf model")
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

	var etcdResult *etcdComparisonResult
	if *etcdCompare {
		result, err := runEtcdComparisonBenchmark(c.clientAddrs, runDir, etcdComparisonConfig{
			WorkloadDuration:   time.Duration(*etcdDurationSec) * time.Second,
			WriteWorkers:       *etcdWriteWorkers,
			ReadWorkers:        *etcdReadWorkers,
			PayloadBytes:       *etcdPayloadBytes,
			ReadKeyspace:       *etcdReadKeyspace,
			LightLoadOps:       *etcdLightOps,
			DiskFsyncSamples:   *etcdDiskFsyncSamples,
			DiskCommitSamples:  *etcdDiskCommitSamples,
			DiskCommitBatch:    *etcdDiskCommitBatch,
			CheckPerfDuration:  time.Duration(*etcdCheckPerfSec) * time.Second,
			CheckPerfKeepTrace: false,
		})
		if err != nil {
			log.Fatalf("etcd comparison benchmark failed: %v", err)
		}
		etcdResult = &result
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
		EtcdComparison: etcdResult,
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
	if etcdResult != nil {
		fmt.Printf("  etcd write (leader-targeted): %.0f req/s, mean %.2fms (target 44k req/s, 22ms)\n", etcdResult.WriteLeader.ThroughputRPS, etcdResult.WriteLeader.MeanLatencyMS)
		fmt.Printf("  etcd write (all-members): %.0f req/s, mean %.2fms (target 50k req/s, 20ms)\n", etcdResult.WriteAllMembers.ThroughputRPS, etcdResult.WriteAllMembers.MeanLatencyMS)
		fmt.Printf("  etcd read (linearizable): %.0f req/s, mean %.2fms (target 141k req/s, 5.5ms)\n", etcdResult.ReadLinearizable.ThroughputRPS, etcdResult.ReadLinearizable.MeanLatencyMS)
		fmt.Printf("  etcd read (serializable): %.0f req/s, mean %.2fms (target 186k req/s, 2.2ms)\n", etcdResult.ReadSerializable.ThroughputRPS, etcdResult.ReadSerializable.MeanLatencyMS)
		fmt.Printf("  disk SLO proxy: wal p99 %.2fms (<10ms), backend commit p99 %.2fms (<25ms)\n", etcdResult.DiskLatency.WALFsync.P99Latency, etcdResult.DiskLatency.BackendCommit.P99Latency)
		fmt.Printf("  etcd comparison targets met: %v\n", etcdResult.AllTargetsMet)
	}
	fmt.Printf("  artifacts dir: %s\n", runDir)

	if *strict {
		allTargets := checks.AllTargetsMet
		if etcdResult != nil {
			allTargets = allTargets && etcdResult.AllTargetsMet
		}
		if !allTargets {
			os.Exit(1)
		}
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

func runEtcdComparisonBenchmark(addresses []string, runDir string, cfg etcdComparisonConfig) (etcdComparisonResult, error) {
	if len(addresses) == 0 {
		return etcdComparisonResult{}, fmt.Errorf("at least one address is required")
	}
	if cfg.WorkloadDuration <= 0 {
		return etcdComparisonResult{}, fmt.Errorf("workload duration must be > 0")
	}
	if cfg.WriteWorkers <= 0 || cfg.ReadWorkers <= 0 {
		return etcdComparisonResult{}, fmt.Errorf("write/read workers must be > 0")
	}
	if cfg.PayloadBytes <= 0 {
		return etcdComparisonResult{}, fmt.Errorf("payload bytes must be > 0")
	}
	if cfg.ReadKeyspace <= 0 {
		return etcdComparisonResult{}, fmt.Errorf("read keyspace must be > 0")
	}
	if cfg.LightLoadOps <= 0 {
		return etcdComparisonResult{}, fmt.Errorf("light load ops must be > 0")
	}
	if cfg.DiskFsyncSamples <= 0 || cfg.DiskCommitSamples <= 0 || cfg.DiskCommitBatch <= 0 {
		return etcdComparisonResult{}, fmt.Errorf("disk sample counts and commit batch must be > 0")
	}
	if cfg.CheckPerfDuration <= 0 {
		return etcdComparisonResult{}, fmt.Errorf("check-perf duration must be > 0")
	}

	leader, err := discoverLeader(addresses, 20*time.Second)
	if err != nil {
		return etcdComparisonResult{}, fmt.Errorf("leader discovery failed: %w", err)
	}

	log.Printf("etcd compare: preloading %d keys for read-heavy scenarios", cfg.ReadKeyspace)
	readKeys, leader, err := preloadReadDataset(addresses, leader, cfg.ReadKeyspace, cfg.PayloadBytes)
	if err != nil {
		return etcdComparisonResult{}, fmt.Errorf("preload read dataset failed: %w", err)
	}

	log.Printf("etcd compare: running heavy write (leader-targeted)")
	writeLeader, err := runLoadScenario(loadScenarioConfig{
		Name:         "write_leader_targeted",
		Mode:         loadModeWriteLeader,
		Addresses:    addresses,
		Duration:     cfg.WorkloadDuration,
		Workers:      cfg.WriteWorkers,
		PayloadBytes: cfg.PayloadBytes,
		LeaderHint:   leader,
		KeyStart:     0x20000000,
		KeepTrace:    false,
	})
	if err != nil {
		return etcdComparisonResult{}, err
	}

	log.Printf("etcd compare: running heavy write (all-members targeted)")
	writeAllMembers, err := runLoadScenario(loadScenarioConfig{
		Name:         "write_all_members_targeted",
		Mode:         loadModeWriteAllMembers,
		Addresses:    addresses,
		Duration:     cfg.WorkloadDuration,
		Workers:      cfg.WriteWorkers,
		PayloadBytes: cfg.PayloadBytes,
		LeaderHint:   leader,
		KeyStart:     0x30000000,
		KeepTrace:    false,
	})
	if err != nil {
		return etcdComparisonResult{}, err
	}

	log.Printf("etcd compare: running heavy read (linearizable approximation via leader reads)")
	readLinearizable, err := runLoadScenario(loadScenarioConfig{
		Name:       "read_linearizable",
		Mode:       loadModeReadLeader,
		Addresses:  addresses,
		Duration:   cfg.WorkloadDuration,
		Workers:    cfg.ReadWorkers,
		ReadKeys:   readKeys,
		LeaderHint: leader,
		KeepTrace:  false,
	})
	if err != nil {
		return etcdComparisonResult{}, err
	}

	log.Printf("etcd compare: running heavy read (serializable approximation via any-member reads)")
	readSerializable, err := runLoadScenario(loadScenarioConfig{
		Name:       "read_serializable",
		Mode:       loadModeReadAllMembers,
		Addresses:  addresses,
		Duration:   cfg.WorkloadDuration,
		Workers:    cfg.ReadWorkers,
		ReadKeys:   readKeys,
		LeaderHint: leader,
		KeepTrace:  false,
	})
	if err != nil {
		return etcdComparisonResult{}, err
	}

	// Heavy concurrent phases can transiently disturb leadership; re-discover
	// the writable endpoint before the sequential light-load probe.
	time.Sleep(1500 * time.Millisecond)
	leader, err = discoverLeader(addresses, 20*time.Second)
	if err != nil {
		return etcdComparisonResult{}, fmt.Errorf("leader discovery before light-load probe failed: %w", err)
	}

	log.Printf("etcd compare: running light-load latency probe")
	lightPut, lightGet, err := runLightLoadProbe(addresses, leader, cfg.PayloadBytes, cfg.LightLoadOps)
	if err != nil {
		return etcdComparisonResult{}, err
	}

	log.Printf("etcd compare: running disk-latency probe")
	diskLatency, err := runDiskLatencyProbe(runDir, cfg.PayloadBytes, cfg.DiskFsyncSamples, cfg.DiskCommitSamples, cfg.DiskCommitBatch)
	if err != nil {
		return etcdComparisonResult{}, err
	}

	log.Printf("etcd compare: running etcdctl check-perf style gates")
	checkPerf, err := runCheckPerfModels(addresses, leader, cfg.PayloadBytes, cfg.CheckPerfDuration, cfg.CheckPerfKeepTrace)
	if err != nil {
		return etcdComparisonResult{}, err
	}

	targetChecks := []etcdTargetCheck{
		{
			Name:       "write_leader_throughput_rps",
			Actual:     writeLeader.ThroughputRPS,
			Target:     etcdTargetWriteLeaderRPS,
			Comparator: ">=",
			Pass:       writeLeader.ThroughputRPS >= etcdTargetWriteLeaderRPS,
		},
		{
			Name:       "write_leader_mean_latency_ms",
			Actual:     writeLeader.MeanLatencyMS,
			Target:     etcdTargetWriteLeaderMeanMS,
			Comparator: "<=",
			Pass:       writeLeader.MeanLatencyMS <= etcdTargetWriteLeaderMeanMS,
		},
		{
			Name:       "write_all_members_throughput_rps",
			Actual:     writeAllMembers.ThroughputRPS,
			Target:     etcdTargetWriteAllRPS,
			Comparator: ">=",
			Pass:       writeAllMembers.ThroughputRPS >= etcdTargetWriteAllRPS,
		},
		{
			Name:       "write_all_members_mean_latency_ms",
			Actual:     writeAllMembers.MeanLatencyMS,
			Target:     etcdTargetWriteAllMeanMS,
			Comparator: "<=",
			Pass:       writeAllMembers.MeanLatencyMS <= etcdTargetWriteAllMeanMS,
		},
		{
			Name:       "read_linearizable_throughput_rps",
			Actual:     readLinearizable.ThroughputRPS,
			Target:     etcdTargetReadLinearRPS,
			Comparator: ">=",
			Pass:       readLinearizable.ThroughputRPS >= etcdTargetReadLinearRPS,
		},
		{
			Name:       "read_linearizable_mean_latency_ms",
			Actual:     readLinearizable.MeanLatencyMS,
			Target:     etcdTargetReadLinearMeanMS,
			Comparator: "<=",
			Pass:       readLinearizable.MeanLatencyMS <= etcdTargetReadLinearMeanMS,
		},
		{
			Name:       "read_serializable_throughput_rps",
			Actual:     readSerializable.ThroughputRPS,
			Target:     etcdTargetReadSerialRPS,
			Comparator: ">=",
			Pass:       readSerializable.ThroughputRPS >= etcdTargetReadSerialRPS,
		},
		{
			Name:       "read_serializable_mean_latency_ms",
			Actual:     readSerializable.MeanLatencyMS,
			Target:     etcdTargetReadSerialMeanMS,
			Comparator: "<=",
			Pass:       readSerializable.MeanLatencyMS <= etcdTargetReadSerialMeanMS,
		},
		{
			Name:       "light_load_put_mean_latency_ms",
			Actual:     lightPut.MeanLatency,
			Target:     etcdTargetLightLatencyMS,
			Comparator: "<=",
			Pass:       lightPut.MeanLatency <= etcdTargetLightLatencyMS,
		},
		{
			Name:       "light_load_get_mean_latency_ms",
			Actual:     lightGet.MeanLatency,
			Target:     etcdTargetLightLatencyMS,
			Comparator: "<=",
			Pass:       lightGet.MeanLatency <= etcdTargetLightLatencyMS,
		},
		{
			Name:       "wal_fsync_p99_ms",
			Actual:     diskLatency.WALFsync.P99Latency,
			Target:     etcdTargetWALFsyncP99MS,
			Comparator: "<=",
			Pass:       diskLatency.WALP99UnderTarget,
		},
		{
			Name:       "backend_commit_p99_ms",
			Actual:     diskLatency.BackendCommit.P99Latency,
			Target:     etcdTargetBackendCommitP99MS,
			Comparator: "<=",
			Pass:       diskLatency.BackendCommitUnderTarget,
		},
	}

	allTargets := true
	for _, check := range targetChecks {
		allTargets = allTargets && check.Pass
	}
	for _, model := range checkPerf {
		allTargets = allTargets && model.AllPass
	}

	return etcdComparisonResult{
		KeyBytes:         8,
		ValueBytes:       cfg.PayloadBytes,
		ReadKeyspace:     cfg.ReadKeyspace,
		WorkloadSeconds:  int(cfg.WorkloadDuration / time.Second),
		WriteLeader:      writeLeader,
		WriteAllMembers:  writeAllMembers,
		ReadLinearizable: readLinearizable,
		ReadSerializable: readSerializable,
		LightLoadPut:     lightPut,
		LightLoadGet:     lightGet,
		DiskLatency:      diskLatency,
		CheckPerf:        checkPerf,
		TargetChecks:     targetChecks,
		AllTargetsMet:    allTargets,
	}, nil
}

func preloadReadDataset(addresses []string, leaderHint string, keyCount, payloadBytes int) ([]string, string, error) {
	keys := make([]string, 0, keyCount)

	leader := leaderHint
	writeClient, err := newPersistentClient(leader, 3*time.Second)
	if err != nil {
		return nil, "", err
	}
	defer writeClient.close()

	for i := 0; i < keyCount; i++ {
		key := fixedKey(uint64(i))
		value := fixedPayload(payloadBytes, uint64(i))
		_, leaderOut, err := putWithRetryPersistent(addresses, &leader, &writeClient, key, value)
		if err != nil {
			return nil, "", err
		}
		leader = leaderOut
		keys = append(keys, key)
	}

	// Ensure read-any-member paths observe settled values before read-heavy tests start.
	samples := 32
	if samples > len(keys) {
		samples = len(keys)
	}
	for _, key := range keys[len(keys)-samples:] {
		if err := waitForReplication(addresses, key, fixedPayload(payloadBytes, uint64(parseHexKey(key))), 4*time.Second); err != nil {
			return nil, "", err
		}
	}

	return keys, leader, nil
}

func runLoadScenario(cfg loadScenarioConfig) (loadScenarioResult, error) {
	if len(cfg.Addresses) == 0 {
		return loadScenarioResult{}, fmt.Errorf("%s: no addresses provided", cfg.Name)
	}
	if cfg.Duration <= 0 {
		return loadScenarioResult{}, fmt.Errorf("%s: duration must be > 0", cfg.Name)
	}
	if cfg.Workers <= 0 {
		return loadScenarioResult{}, fmt.Errorf("%s: workers must be > 0", cfg.Name)
	}
	if (cfg.Mode == loadModeReadLeader || cfg.Mode == loadModeReadAllMembers) && len(cfg.ReadKeys) == 0 {
		return loadScenarioResult{}, fmt.Errorf("%s: read mode requires read keys", cfg.Name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	var keyCounter uint64 = cfg.KeyStart
	var roundRobin uint64
	var leaderRef atomic.Value
	leaderRef.Store(cfg.LeaderHint)

	resultsCh := make(chan workerMetrics, cfg.Workers)
	wg := sync.WaitGroup{}
	startWall := time.Now()

	for workerID := 0; workerID < cfg.Workers; workerID++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*7919))
			pool := newWorkerClientPool(etcdLoadRequestTimeout)
			defer pool.close()

			metrics := workerMetrics{latencies: make([]float64, 0, 1024)}
			for {
				select {
				case <-ctx.Done():
					resultsCh <- metrics
					return
				default:
				}

				opStart := time.Now()
				success := executeLoadOperation(pool, cfg, &leaderRef, &keyCounter, &roundRobin, rng)
				metrics.latencies = append(metrics.latencies, float64(time.Since(opStart).Microseconds())/1000.0)
				if success {
					metrics.successes++
				} else {
					metrics.failures++
				}
			}
		}(workerID)
	}

	wg.Wait()
	close(resultsCh)
	elapsed := time.Since(startWall)

	totalSuccess := 0
	totalFailures := 0
	allLatencies := make([]float64, 0, cfg.Workers*1024)
	for metrics := range resultsCh {
		totalSuccess += metrics.successes
		totalFailures += metrics.failures
		allLatencies = append(allLatencies, metrics.latencies...)
	}

	return summarizeLoadScenario(cfg.Name, cfg.Workers, elapsed, totalSuccess, totalFailures, allLatencies, cfg.KeepTrace), nil
}

func executeLoadOperation(pool *workerClientPool, cfg loadScenarioConfig, leaderRef *atomic.Value, keyCounter, roundRobin *uint64, rng *rand.Rand) bool {
	switch cfg.Mode {
	case loadModeWriteLeader:
		keyID := atomic.AddUint64(keyCounter, 1) - 1
		key := fixedKey(keyID)
		return writeLeader(pool, cfg.Addresses, leaderRef, key, fixedPayload(cfg.PayloadBytes, keyID))
	case loadModeWriteAllMembers:
		keyID := atomic.AddUint64(keyCounter, 1) - 1
		key := fixedKey(keyID)
		target := cfg.Addresses[int((atomic.AddUint64(roundRobin, 1)-1)%uint64(len(cfg.Addresses)))]
		return writeAllMembers(pool, cfg.Addresses, leaderRef, target, key, fixedPayload(cfg.PayloadBytes, keyID))
	case loadModeReadLeader:
		key := cfg.ReadKeys[rng.Intn(len(cfg.ReadKeys))]
		return readLeader(pool, cfg.Addresses, leaderRef, key)
	case loadModeReadAllMembers:
		key := cfg.ReadKeys[rng.Intn(len(cfg.ReadKeys))]
		target := cfg.Addresses[rng.Intn(len(cfg.Addresses))]
		return readTarget(pool, target, key)
	default:
		return false
	}
}

func writeLeader(pool *workerClientPool, addresses []string, leaderRef *atomic.Value, key, value string) bool {
	const maxAttempts = 10

	for attempt := 0; attempt < maxAttempts; attempt++ {
		leader := leaderRef.Load().(string)
		if leader == "" {
			discovered, err := discoverLeader(addresses, 2*time.Second)
			if err != nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			leader = discovered
			leaderRef.Store(leader)
		}

		resp, err := pool.request(leader, ClientRequest{Type: "put", Key: key, Value: value})
		if err == nil && resp.Success {
			leaderRef.Store(leader)
			return true
		}
		if err != nil || isNotLeader(resp.Error) {
			leaderRef.Store("")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func writeAllMembers(pool *workerClientPool, addresses []string, leaderRef *atomic.Value, firstTarget, key, value string) bool {
	resp, err := pool.request(firstTarget, ClientRequest{Type: "put", Key: key, Value: value})
	if err == nil && resp.Success {
		return true
	}
	if err != nil || isNotLeader(resp.Error) {
		return writeLeader(pool, addresses, leaderRef, key, value)
	}
	return false
}

func readLeader(pool *workerClientPool, addresses []string, leaderRef *atomic.Value, key string) bool {
	const maxAttempts = 10

	for attempt := 0; attempt < maxAttempts; attempt++ {
		leader := leaderRef.Load().(string)
		if leader == "" {
			discovered, err := discoverLeader(addresses, 2*time.Second)
			if err != nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			leader = discovered
			leaderRef.Store(leader)
		}

		resp, err := pool.request(leader, ClientRequest{Type: "get", Key: key})
		if err == nil && resp.Success {
			return true
		}
		if err != nil || isNotLeader(resp.Error) {
			leaderRef.Store("")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func readTarget(pool *workerClientPool, target, key string) bool {
	resp, err := pool.request(target, ClientRequest{Type: "get", Key: key})
	return err == nil && resp.Success
}

func runLightLoadProbe(addresses []string, leader string, payloadBytes, ops int) (latencySummary, latencySummary, error) {
	var leaderRef atomic.Value
	leaderRef.Store(leader)

	pool := newWorkerClientPool(etcdLoadRequestTimeout)
	defer pool.close()

	putLatencies := make([]float64, 0, ops)
	getLatencies := make([]float64, 0, ops)

	for i := 0; i < ops; i++ {
		keyID := uint64(0x70000000 + i)
		key := fixedKey(keyID)
		value := fixedPayload(payloadBytes, keyID)

		putStart := time.Now()
		if !writeLeader(pool, addresses, &leaderRef, key, value) {
			return latencySummary{}, latencySummary{}, fmt.Errorf("light-load put failed at iteration %d", i)
		}
		putLatencies = append(putLatencies, float64(time.Since(putStart).Microseconds())/1000.0)

		getStart := time.Now()
		if !readLeader(pool, addresses, &leaderRef, key) {
			return latencySummary{}, latencySummary{}, fmt.Errorf("light-load get failed at iteration %d", i)
		}
		getLatencies = append(getLatencies, float64(time.Since(getStart).Microseconds())/1000.0)
	}

	return summarizeLatency("light_load_put", putLatencies, true), summarizeLatency("light_load_get", getLatencies, true), nil
}

func runDiskLatencyProbe(runDir string, payloadBytes, walSamples, commitSamples, commitBatch int) (diskLatencyResult, error) {
	walDir := filepath.Join(runDir, "disk_probe_wal")
	if err := os.RemoveAll(walDir); err != nil {
		return diskLatencyResult{}, err
	}

	walDB, err := kvstore.OpenWithOptions(walDir, kvstore.OpenOptions{
		ReadWrite:            true,
		SyncOnPut:            true,
		MaxDataFileSizeBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		return diskLatencyResult{}, err
	}

	walLatencies := make([]float64, 0, walSamples)
	for i := 0; i < walSamples; i++ {
		keyID := uint64(0x80000000 + i)
		start := time.Now()
		if err := walDB.Put(fixedKey(keyID), fixedPayload(payloadBytes, keyID)); err != nil {
			_ = walDB.Close()
			return diskLatencyResult{}, err
		}
		walLatencies = append(walLatencies, float64(time.Since(start).Microseconds())/1000.0)
	}
	if err := walDB.Close(); err != nil {
		return diskLatencyResult{}, err
	}

	commitDir := filepath.Join(runDir, "disk_probe_commit")
	if err := os.RemoveAll(commitDir); err != nil {
		return diskLatencyResult{}, err
	}

	commitDB, err := kvstore.OpenWithOptions(commitDir, kvstore.OpenOptions{
		ReadWrite:            true,
		SyncOnPut:            false,
		MaxDataFileSizeBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		return diskLatencyResult{}, err
	}

	commitLatencies := make([]float64, 0, commitSamples)
	keyID := uint64(0x90000000)
	for i := 0; i < commitSamples; i++ {
		for j := 0; j < commitBatch; j++ {
			if err := commitDB.Put(fixedKey(keyID), fixedPayload(payloadBytes, keyID)); err != nil {
				_ = commitDB.Close()
				return diskLatencyResult{}, err
			}
			keyID++
		}

		start := time.Now()
		if err := commitDB.Sync(); err != nil {
			_ = commitDB.Close()
			return diskLatencyResult{}, err
		}
		commitLatencies = append(commitLatencies, float64(time.Since(start).Microseconds())/1000.0)
	}
	if err := commitDB.Close(); err != nil {
		return diskLatencyResult{}, err
	}

	walSummary := summarizeLatency("wal_fsync", walLatencies, true)
	commitSummary := summarizeLatency("backend_commit", commitLatencies, true)
	return diskLatencyResult{
		WALFsync:                 walSummary,
		BackendCommit:            commitSummary,
		WALP99TargetMS:           etcdTargetWALFsyncP99MS,
		BackendCommitTargetMS:    etcdTargetBackendCommitP99MS,
		WALP99UnderTarget:        walSummary.P99Latency <= etcdTargetWALFsyncP99MS,
		BackendCommitUnderTarget: commitSummary.P99Latency <= etcdTargetBackendCommitP99MS,
	}, nil
}

func runCheckPerfModels(addresses []string, leader string, payloadBytes int, duration time.Duration, keepTrace bool) ([]checkPerfModelResult, error) {
	models := []checkPerfModelConfig{
		{Name: "small", MinThroughputRPS: 135, Workers: 4},
		{Name: "medium", MinThroughputRPS: 900, Workers: 8},
		{Name: "large", MinThroughputRPS: 7200, Workers: 32},
		{Name: "xlarge", MinThroughputRPS: 13500, Workers: 64},
	}

	results := make([]checkPerfModelResult, 0, len(models))
	keyStart := uint64(0xA0000000)
	for _, model := range models {
		res, err := runLoadScenario(loadScenarioConfig{
			Name:         "check_perf_" + model.Name,
			Mode:         loadModeWriteLeader,
			Addresses:    addresses,
			Duration:     duration,
			Workers:      model.Workers,
			PayloadBytes: payloadBytes,
			LeaderHint:   leader,
			KeyStart:     keyStart,
			KeepTrace:    keepTrace,
		})
		if err != nil {
			return nil, err
		}
		keyStart += 0x01000000

		throughputPass := res.ThroughputRPS >= model.MinThroughputRPS
		slowestPass := res.SlowestLatencyMS <= etcdCheckPerfSlowestMaxMS
		stdDevPass := res.StdDevLatencyMS <= etcdCheckPerfStdDevMaxMS
		results = append(results, checkPerfModelResult{
			Model:              model.Name,
			MinThroughputRPS:   model.MinThroughputRPS,
			SlowestTargetMS:    etcdCheckPerfSlowestMaxMS,
			StdDevTargetMS:     etcdCheckPerfStdDevMaxMS,
			Result:             res,
			ThroughputPass:     throughputPass,
			SlowestLatencyPass: slowestPass,
			LatencyStdDevPass:  stdDevPass,
			AllPass:            throughputPass && slowestPass && stdDevPass,
		})
	}

	return results, nil
}

func summarizeLoadScenario(name string, workers int, duration time.Duration, successes, failures int, latencies []float64, keepTrace bool) loadScenarioResult {
	summary := summarizeLatency(name, latencies, keepTrace)
	durationSeconds := duration.Seconds()
	if durationSeconds <= 0 {
		durationSeconds = 1
	}

	result := loadScenarioResult{
		Name:             name,
		Workers:          workers,
		DurationSeconds:  durationSeconds,
		Requests:         successes + failures,
		Successes:        successes,
		Failures:         failures,
		ThroughputRPS:    float64(successes) / durationSeconds,
		MeanLatencyMS:    summary.MeanLatency,
		P50LatencyMS:     summary.P50Latency,
		P95LatencyMS:     summary.P95Latency,
		P99LatencyMS:     summary.P99Latency,
		SlowestLatencyMS: summary.MaxLatency,
		StdDevLatencyMS:  summary.StdDev,
	}
	if keepTrace {
		result.LatencySamplesMS = summary.LatencyTrace
	}
	return result
}

func summarizeLatency(_ string, samples []float64, keepTrace bool) latencySummary {
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)

	summary := latencySummary{
		Samples:     len(sorted),
		MeanLatency: mean(sorted),
		P50Latency:  percentile(sorted, 0.50),
		P95Latency:  percentile(sorted, 0.95),
		P99Latency:  percentile(sorted, 0.99),
		StdDev:      stdDev(sorted),
	}
	if len(sorted) > 0 {
		summary.MaxLatency = sorted[len(sorted)-1]
	}
	if keepTrace {
		summary.LatencyTrace = sorted
	}
	return summary
}

func stdDev(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	m := mean(values)
	sum := 0.0
	for _, v := range values {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(values)))
}

func isNotLeader(errText string) bool {
	return strings.Contains(strings.ToLower(errText), "not leader")
}

func fixedKey(id uint64) string {
	return fmt.Sprintf("%08x", uint32(id))
}

func fixedPayload(payloadBytes int, seed uint64) string {
	if payloadBytes <= 0 {
		return ""
	}
	prefix := fmt.Sprintf("%08x", uint32(seed))
	if payloadBytes <= len(prefix) {
		return prefix[:payloadBytes]
	}
	return prefix + strings.Repeat("v", payloadBytes-len(prefix))
}

type workerClientPool struct {
	timeout time.Duration
	clients map[string]*persistentClient
}

func newWorkerClientPool(timeout time.Duration) *workerClientPool {
	return &workerClientPool{
		timeout: timeout,
		clients: make(map[string]*persistentClient),
	}
}

func (p *workerClientPool) request(address string, req ClientRequest) (ClientResponse, error) {
	client := p.clients[address]
	if client == nil {
		c, err := newPersistentClient(address, p.timeout)
		if err != nil {
			return ClientResponse{}, err
		}
		p.clients[address] = c
		client = c
	}

	resp, _, err := client.request(req)
	if err != nil {
		_ = client.close()
		delete(p.clients, address)
		return ClientResponse{}, err
	}
	return resp, nil
}

func (p *workerClientPool) close() {
	for address, client := range p.clients {
		_ = client.close()
		delete(p.clients, address)
	}
}

func parseHexKey(key string) uint64 {
	parsed, err := strconv.ParseUint(key, 16, 32)
	if err != nil {
		return 0
	}
	return parsed
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
