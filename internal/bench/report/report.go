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
	GeneratedAtUTC string             `json:"generated_at_utc"`
	Raft           RaftBenchResult    `json:"raft"`
	Storage        StorageBenchResult `json:"storage"`
	Checks         ClaimChecks        `json:"checks"`
}

func SummarizeLatency(samples []float64) []float64 {
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	return sorted
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
