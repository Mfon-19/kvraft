# Performance Baseline (M0)

This baseline was captured on **2026-03-07** on branch `refactor-core` before refactor milestones M1-M7.

## Baseline command

```bash
go build -o kv-server ./cmd/kv-server
go run ./cmd/kv-bench \
  -server-bin ./kv-server \
  -etcd-compare \
  -etcd-duration-sec 15 \
  -etcd-check-perf-sec 8 \
  -etcd-write-workers 64 \
  -etcd-read-workers 96 \
  -etcd-read-keyspace 5000 \
  -etcd-payload-bytes 256 \
  -etcd-light-ops 200 \
  -dataset-keys 12000 \
  -dataset-rounds 2 \
  -dataset-payload-bytes 512 \
  -restart-trials 3 \
  -keep-artifacts \
  -workdir benchmark-artifacts/refactor-core/m0-baseline-r2
```

Raw report: `benchmark-artifacts/refactor-core/m0-baseline.json`
Baseline summary: `docs/perf-baseline.json`

## No-regression gates

- Throughput metrics must be `>= 90%` of baseline.
- Latency metrics must be `<= 115%` of baseline.
- Disk SLO proxies must remain:
  - WAL fsync p99 `< 10ms`
  - backend commit p99 `< 25ms`

These gates are enforced in benchmark compare mode introduced during M6.

## Smoke baseline for CI

For CI runtime, a reduced workload baseline is tracked at `docs/perf-baseline-smoke.json`.
The benchmark runner compares current results against that file when `-baseline` is set.
