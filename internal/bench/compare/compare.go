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
		ThroughputRatioMin float64 `json:"throughput_ratio_min"`
		LatencyRatioMax    float64 `json:"latency_ratio_max"`
		WALP99MSMax        float64 `json:"wal_p99_ms_max"`
		BackendP99MSMax    float64 `json:"backend_commit_p99_ms_max"`
	} `json:"gates"`
	Metrics struct {
		RaftWriteP99MS                float64 `json:"raft_write_p99_ms"`
		WriteLeaderThroughputRPS      float64 `json:"write_leader_throughput_rps"`
		WriteAllMembersThroughputRPS  float64 `json:"write_all_members_throughput_rps"`
		ReadLinearizableThroughputRPS float64 `json:"read_linearizable_throughput_rps"`
		ReadSerializableThroughputRPS float64 `json:"read_serializable_throughput_rps"`
		WriteLeaderMeanLatencyMS      float64 `json:"write_leader_mean_latency_ms"`
		WriteAllMembersMeanLatencyMS  float64 `json:"write_all_members_mean_latency_ms"`
		ReadLinearizableMeanLatencyMS float64 `json:"read_linearizable_mean_latency_ms"`
		ReadSerializableMeanLatencyMS float64 `json:"read_serializable_mean_latency_ms"`
		LightPutMeanLatencyMS         float64 `json:"light_put_mean_latency_ms"`
		LightGetMeanLatencyMS         float64 `json:"light_get_mean_latency_ms"`
		WALFsyncP99MS                 float64 `json:"wal_fsync_p99_ms"`
		BackendCommitP99MS            float64 `json:"backend_commit_p99_ms"`
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
	if b.Gates.ThroughputRatioMin <= 0 {
		b.Gates.ThroughputRatioMin = 0.9
	}
	if b.Gates.LatencyRatioMax <= 0 {
		b.Gates.LatencyRatioMax = 1.15
	}
	if b.Gates.WALP99MSMax <= 0 {
		b.Gates.WALP99MSMax = 10
	}
	if b.Gates.BackendP99MSMax <= 0 {
		b.Gates.BackendP99MSMax = 25
	}
	return b, nil
}

func Compare(current benchreport.BenchmarkReport, baselinePath string, base Baseline) Result {
	result := Result{BaselinePath: baselinePath, Checks: make([]Check, 0)}
	addThroughput := func(name string, actual, baseline float64) {
		if baseline <= 0 {
			return
		}
		threshold := baseline * base.Gates.ThroughputRatioMin
		result.Checks = append(result.Checks, Check{
			Name:       name,
			Actual:     actual,
			Threshold:  threshold,
			Comparator: ">=",
			Pass:       actual >= threshold,
		})
	}
	addLatency := func(name string, actual, baseline float64) {
		if baseline <= 0 {
			return
		}
		threshold := baseline * base.Gates.LatencyRatioMax
		result.Checks = append(result.Checks, Check{
			Name:       name,
			Actual:     actual,
			Threshold:  threshold,
			Comparator: "<=",
			Pass:       actual <= threshold,
		})
	}

	addLatency("raft_write_p99_ms", current.Raft.P99LatencyMS, base.Metrics.RaftWriteP99MS)

	if current.EtcdComparison != nil {
		etcd := current.EtcdComparison
		addThroughput("write_leader_throughput_rps", etcd.WriteLeader.ThroughputRPS, base.Metrics.WriteLeaderThroughputRPS)
		addThroughput("write_all_members_throughput_rps", etcd.WriteAllMembers.ThroughputRPS, base.Metrics.WriteAllMembersThroughputRPS)
		addThroughput("read_linearizable_throughput_rps", etcd.ReadLinearizable.ThroughputRPS, base.Metrics.ReadLinearizableThroughputRPS)
		addThroughput("read_serializable_throughput_rps", etcd.ReadSerializable.ThroughputRPS, base.Metrics.ReadSerializableThroughputRPS)

		addLatency("write_leader_mean_latency_ms", etcd.WriteLeader.MeanLatencyMS, base.Metrics.WriteLeaderMeanLatencyMS)
		addLatency("write_all_members_mean_latency_ms", etcd.WriteAllMembers.MeanLatencyMS, base.Metrics.WriteAllMembersMeanLatencyMS)
		addLatency("read_linearizable_mean_latency_ms", etcd.ReadLinearizable.MeanLatencyMS, base.Metrics.ReadLinearizableMeanLatencyMS)
		addLatency("read_serializable_mean_latency_ms", etcd.ReadSerializable.MeanLatencyMS, base.Metrics.ReadSerializableMeanLatencyMS)
		addLatency("light_put_mean_latency_ms", etcd.LightLoadPut.MeanLatency, base.Metrics.LightPutMeanLatencyMS)
		addLatency("light_get_mean_latency_ms", etcd.LightLoadGet.MeanLatency, base.Metrics.LightGetMeanLatencyMS)

		result.Checks = append(result.Checks,
			Check{
				Name:       "wal_fsync_p99_ms",
				Actual:     etcd.DiskLatency.WALFsync.P99Latency,
				Threshold:  base.Gates.WALP99MSMax,
				Comparator: "<=",
				Pass:       etcd.DiskLatency.WALFsync.P99Latency < base.Gates.WALP99MSMax,
			},
			Check{
				Name:       "backend_commit_p99_ms",
				Actual:     etcd.DiskLatency.BackendCommit.P99Latency,
				Threshold:  base.Gates.BackendP99MSMax,
				Comparator: "<=",
				Pass:       etcd.DiskLatency.BackendCommit.P99Latency < base.Gates.BackendP99MSMax,
			},
		)
	}

	result.AllPass = true
	for _, c := range result.Checks {
		result.AllPass = result.AllPass && c.Pass
	}
	return result
}
