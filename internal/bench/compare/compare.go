package compare

import (
	"encoding/json"
	"fmt"
	"os"

	benchreport "kvraft/internal/bench/report"
)

type Baseline struct {
	CapturedAtUTC string `json:"captured_at_utc"`
	Command       string `json:"benchmark_command"`
	SourceReport  string `json:"source_report"`
	Gates         struct {
		LatencyRatioMax       float64 `json:"latency_ratio_max"`
		RestartRatioMax       float64 `json:"restart_ratio_max"`
		DiskReductionDeltaMax float64 `json:"disk_reduction_delta_max"`
	} `json:"gates"`
	Metrics struct {
		RaftWriteP99MS   float64 `json:"raft_write_p99_ms"`
		RestartMergedMS  float64 `json:"restart_merged_hints_ms"`
		DiskReductionPct float64 `json:"disk_reduction_percent"`
	} `json:"metrics"`
}

type Check struct {
	Name       string  `json:"name"`
	Actual     float64 `json:"actual"`
	Threshold  float64 `json:"threshold"`
	Comparator string  `json:"comparator"`
	Pass       bool    `json:"pass"`
}

type Result struct {
	BaselinePath string  `json:"baseline_path"`
	Checks       []Check `json:"checks"`
	AllPass      bool    `json:"all_pass"`
}

func LoadBaseline(path string) (Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Baseline{}, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return Baseline{}, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	if b.Gates.LatencyRatioMax <= 0 {
		b.Gates.LatencyRatioMax = 1.15
	}
	if b.Gates.RestartRatioMax <= 0 {
		b.Gates.RestartRatioMax = 1.15
	}
	if b.Gates.DiskReductionDeltaMax <= 0 {
		b.Gates.DiskReductionDeltaMax = 5.0
	}
	return b, nil
}

func Compare(current benchreport.BenchmarkReport, baselinePath string, base Baseline) Result {
	result := Result{BaselinePath: baselinePath, Checks: make([]Check, 0, 3)}

	if base.Metrics.RaftWriteP99MS > 0 {
		threshold := base.Metrics.RaftWriteP99MS * base.Gates.LatencyRatioMax
		result.Checks = append(result.Checks, Check{
			Name:       "raft_write_p99_ms",
			Actual:     current.Raft.P99LatencyMS,
			Threshold:  threshold,
			Comparator: "<=",
			Pass:       current.Raft.P99LatencyMS <= threshold,
		})
	}

	if base.Metrics.RestartMergedMS > 0 {
		threshold := base.Metrics.RestartMergedMS * base.Gates.RestartRatioMax
		result.Checks = append(result.Checks, Check{
			Name:       "restart_merged_hints_ms",
			Actual:     current.Storage.RestartMergedHintsMS,
			Threshold:  threshold,
			Comparator: "<=",
			Pass:       current.Storage.RestartMergedHintsMS <= threshold,
		})
	}

	if base.Metrics.DiskReductionPct > 0 {
		threshold := base.Metrics.DiskReductionPct - base.Gates.DiskReductionDeltaMax
		result.Checks = append(result.Checks, Check{
			Name:       "disk_reduction_percent",
			Actual:     current.Storage.DiskReductionPercent,
			Threshold:  threshold,
			Comparator: ">=",
			Pass:       current.Storage.DiskReductionPercent >= threshold,
		})
	}

	result.AllPass = true
	for _, c := range result.Checks {
		result.AllPass = result.AllPass && c.Pass
	}
	return result
}
