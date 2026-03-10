package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"kvraft/common"
	benchcluster "kvraft/internal/bench/cluster"
	benchcompare "kvraft/internal/bench/compare"
	benchreport "kvraft/internal/bench/report"
	benchstorage "kvraft/internal/bench/storage"
	benchworkload "kvraft/internal/bench/workload"
	pb "kvraft/proto"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kvraft/kvstore"
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
type latencySummary = benchreport.LatencySummary
type loadScenarioResult = benchreport.LoadScenarioResult
type diskLatencyResult = benchreport.DiskLatencyResult
type checkPerfModelResult = benchreport.CheckPerfModelResult
type etcdTargetCheck = benchreport.EtcdTargetCheck
type etcdComparisonResult = benchreport.EtcdComparisonResult

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
		baselinePath            = flag.String("baseline", "", "Optional baseline JSON path for no-regression comparison")
		strictEtcdTargets       = flag.Bool("strict-etcd-targets", false, "When strict mode is enabled, also require etcd reference targets to pass")
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

	var etcdResult *etcdComparisonResult
	if *etcdCompare {
		result, err := runEtcdComparisonBenchmark(c.ClientAddrs, runDir, etcdComparisonConfig{
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
		EtcdComparison: etcdResult,
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
	if etcdResult != nil {
		fmt.Printf("  etcd write (leader-targeted): %.0f req/s, mean %.2fms (target 44k req/s, 22ms)\n", etcdResult.WriteLeader.ThroughputRPS, etcdResult.WriteLeader.MeanLatencyMS)
		fmt.Printf("  etcd write (all-members): %.0f req/s, mean %.2fms (target 50k req/s, 20ms)\n", etcdResult.WriteAllMembers.ThroughputRPS, etcdResult.WriteAllMembers.MeanLatencyMS)
		fmt.Printf("  etcd read (linearizable): %.0f req/s, mean %.2fms (target 141k req/s, 5.5ms)\n", etcdResult.ReadLinearizable.ThroughputRPS, etcdResult.ReadLinearizable.MeanLatencyMS)
		fmt.Printf("  etcd read (serializable): %.0f req/s, mean %.2fms (target 186k req/s, 2.2ms)\n", etcdResult.ReadSerializable.ThroughputRPS, etcdResult.ReadSerializable.MeanLatencyMS)
		fmt.Printf("  disk SLO proxy: wal p99 %.2fms (<10ms), backend commit p99 %.2fms (<25ms)\n", etcdResult.DiskLatency.WALFsync.P99Latency, etcdResult.DiskLatency.BackendCommit.P99Latency)
		fmt.Printf("  etcd comparison targets met: %v\n", etcdResult.AllTargetsMet)
	}
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
			// No-regression mode focuses on latency/throughput drift and disk SLO proxies.
			allTargets = checks.P99UnderTarget && checks.RestartUnderTarget
		}
		if etcdResult != nil && *strictEtcdTargets {
			allTargets = allTargets && etcdResult.AllTargetsMet
		}
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
		if err != nil || benchworkload.IsNotLeader(resp.Error) {
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
		key := benchworkload.FixedKey(uint64(i))
		value := benchworkload.FixedPayload(payloadBytes, uint64(i))
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
		leaderOut, err := waitForCommittedRead(addresses, leader, key, benchworkload.FixedPayload(payloadBytes, uint64(parseHexKey(key))), 4*time.Second)
		if err != nil {
			return nil, "", err
		}
		leader = leaderOut
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
		key := benchworkload.FixedKey(keyID)
		return writeLeader(pool, cfg.Addresses, leaderRef, key, benchworkload.FixedPayload(cfg.PayloadBytes, keyID))
	case loadModeWriteAllMembers:
		keyID := atomic.AddUint64(keyCounter, 1) - 1
		key := benchworkload.FixedKey(keyID)
		target := cfg.Addresses[int((atomic.AddUint64(roundRobin, 1)-1)%uint64(len(cfg.Addresses)))]
		return writeAllMembers(pool, cfg.Addresses, leaderRef, target, key, benchworkload.FixedPayload(cfg.PayloadBytes, keyID))
	case loadModeReadLeader:
		key := cfg.ReadKeys[rng.Intn(len(cfg.ReadKeys))]
		return readLeader(pool, cfg.Addresses, leaderRef, key)
	case loadModeReadAllMembers:
		key := cfg.ReadKeys[rng.Intn(len(cfg.ReadKeys))]
		target := cfg.Addresses[rng.Intn(len(cfg.Addresses))]
		return readAllMembers(pool, cfg.Addresses, leaderRef, target, key)
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

		resp, err := pool.request(leader, common.ClientRequest{Type: common.OpPut, Key: key, Value: value})
		if err == nil && resp.Success {
			leaderRef.Store(leader)
			return true
		}
		if err != nil || benchworkload.IsNotLeader(resp.Error) {
			leaderRef.Store("")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func writeAllMembers(pool *workerClientPool, addresses []string, leaderRef *atomic.Value, firstTarget, key, value string) bool {
	resp, err := pool.request(firstTarget, common.ClientRequest{Type: common.OpPut, Key: key, Value: value})
	if err == nil && resp.Success {
		return true
	}
	if err != nil || benchworkload.IsNotLeader(resp.Error) {
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

		resp, err := pool.request(leader, common.ClientRequest{Type: common.OpGet, Key: key})
		if err == nil && resp.Success {
			return true
		}
		if err != nil || benchworkload.IsNotLeader(resp.Error) {
			leaderRef.Store("")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func readAllMembers(pool *workerClientPool, addresses []string, leaderRef *atomic.Value, firstTarget, key string) bool {
	resp, err := pool.request(firstTarget, common.ClientRequest{Type: common.OpGet, Key: key})
	if err == nil && resp.Success {
		leaderRef.Store(firstTarget)
		return true
	}
	if err != nil || benchworkload.IsNotLeader(resp.Error) {
		leaderRef.Store("")
		return readLeader(pool, addresses, leaderRef, key)
	}
	return false
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
		key := benchworkload.FixedKey(keyID)
		value := benchworkload.FixedPayload(payloadBytes, keyID)

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
		if err := walDB.Put(benchworkload.FixedKey(keyID), benchworkload.FixedPayload(payloadBytes, keyID)); err != nil {
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
			if err := commitDB.Put(benchworkload.FixedKey(keyID), benchworkload.FixedPayload(payloadBytes, keyID)); err != nil {
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

func (p *workerClientPool) request(address string, req common.ClientRequest) (common.ClientResponse, error) {
	client := p.clients[address]
	if client == nil {
		c, err := newPersistentClient(address, p.timeout)
		if err != nil {
			return common.ClientResponse{}, err
		}
		p.clients[address] = c
		client = c
	}

	resp, _, err := client.request(req)
	if err != nil {
		_ = client.close()
		delete(p.clients, address)
		return common.ClientResponse{}, err
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
