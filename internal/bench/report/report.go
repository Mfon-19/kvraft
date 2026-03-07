package report

import (
	"math"
	"sort"
)

type RaftBenchResult struct {
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

type StorageBenchResult struct {
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

type ClaimChecks struct {
	P99UnderTarget           bool    `json:"p99_under_target"`
	RestartUnderTarget       bool    `json:"restart_under_target"`
	DiskReductionWithinRange bool    `json:"disk_reduction_within_range"`
	P99TargetMS              float64 `json:"p99_target_ms"`
	RestartTargetMS          float64 `json:"restart_target_ms"`
	DiskReductionMinPercent  float64 `json:"disk_reduction_min_percent"`
	DiskReductionMaxPercent  float64 `json:"disk_reduction_max_percent"`
	AllTargetsMet            bool    `json:"all_targets_met"`
}

type BenchmarkReport struct {
	GeneratedAtUTC string                `json:"generated_at_utc"`
	Raft           RaftBenchResult       `json:"raft"`
	Storage        StorageBenchResult    `json:"storage"`
	EtcdComparison *EtcdComparisonResult `json:"etcd_comparison,omitempty"`
	Checks         ClaimChecks           `json:"checks"`
}

type LatencySummary struct {
	Samples      int       `json:"samples"`
	MeanLatency  float64   `json:"mean_latency_ms"`
	P50Latency   float64   `json:"p50_latency_ms"`
	P95Latency   float64   `json:"p95_latency_ms"`
	P99Latency   float64   `json:"p99_latency_ms"`
	MaxLatency   float64   `json:"max_latency_ms"`
	StdDev       float64   `json:"stddev_latency_ms"`
	LatencyTrace []float64 `json:"latency_samples_ms,omitempty"`
}

type LoadScenarioResult struct {
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

type DiskLatencyResult struct {
	WALFsync                 LatencySummary `json:"wal_fsync"`
	BackendCommit            LatencySummary `json:"backend_commit"`
	WALP99TargetMS           float64        `json:"wal_p99_target_ms"`
	BackendCommitTargetMS    float64        `json:"backend_commit_target_ms"`
	WALP99UnderTarget        bool           `json:"wal_p99_under_target"`
	BackendCommitUnderTarget bool           `json:"backend_commit_under_target"`
}

type CheckPerfModelResult struct {
	Model              string             `json:"model"`
	MinThroughputRPS   float64            `json:"min_throughput_rps"`
	SlowestTargetMS    float64            `json:"slowest_target_ms"`
	StdDevTargetMS     float64            `json:"stddev_target_ms"`
	Result             LoadScenarioResult `json:"result"`
	ThroughputPass     bool               `json:"throughput_pass"`
	SlowestLatencyPass bool               `json:"slowest_latency_pass"`
	LatencyStdDevPass  bool               `json:"latency_stddev_pass"`
	AllPass            bool               `json:"all_pass"`
}

type EtcdTargetCheck struct {
	Name       string  `json:"name"`
	Actual     float64 `json:"actual"`
	Target     float64 `json:"target"`
	Comparator string  `json:"comparator"`
	Pass       bool    `json:"pass"`
}

type EtcdComparisonResult struct {
	KeyBytes         int                    `json:"key_bytes"`
	ValueBytes       int                    `json:"value_bytes"`
	ReadKeyspace     int                    `json:"read_keyspace"`
	WorkloadSeconds  int                    `json:"workload_seconds"`
	WriteLeader      LoadScenarioResult     `json:"write_leader_targeted"`
	WriteAllMembers  LoadScenarioResult     `json:"write_all_members_targeted"`
	ReadLinearizable LoadScenarioResult     `json:"read_linearizable"`
	ReadSerializable LoadScenarioResult     `json:"read_serializable"`
	LightLoadPut     LatencySummary         `json:"light_load_put"`
	LightLoadGet     LatencySummary         `json:"light_load_get"`
	DiskLatency      DiskLatencyResult      `json:"disk_latency"`
	CheckPerf        []CheckPerfModelResult `json:"check_perf"`
	TargetChecks     []EtcdTargetCheck      `json:"target_checks"`
	AllTargetsMet    bool                   `json:"all_targets_met"`
}

func SummarizeLoadScenario(name string, workers int, durationSeconds float64, successes, failures int, latencies []float64, keepTrace bool) LoadScenarioResult {
	summary := SummarizeLatency(latencies, keepTrace)
	if durationSeconds <= 0 {
		durationSeconds = 1
	}

	result := LoadScenarioResult{
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

func SummarizeLatency(samples []float64, keepTrace bool) LatencySummary {
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)

	summary := LatencySummary{
		Samples:     len(sorted),
		MeanLatency: Mean(sorted),
		P50Latency:  Percentile(sorted, 0.50),
		P95Latency:  Percentile(sorted, 0.95),
		P99Latency:  Percentile(sorted, 0.99),
		StdDev:      StdDev(sorted),
	}
	if len(sorted) > 0 {
		summary.MaxLatency = sorted[len(sorted)-1]
	}
	if keepTrace {
		summary.LatencyTrace = sorted
	}
	return summary
}

func Percentile(sorted []float64, q float64) float64 {
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

func Mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func StdDev(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	m := Mean(values)
	sum := 0.0
	for _, v := range values {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(values)))
}
