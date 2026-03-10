# Raft-KV

A distributed key-value store written in Go that combines a Raft replication layer with a Bitcask-style storage engine. The system exposes a gRPC API for `Get`, `Put`, and `Delete`, replicates writes across a 3-node quorum for strong write consistency, and uses an append-only on-disk format with an in-memory keydir for low-latency reads.

## Bitcask Primer

Bitcask is a log-structured key-value storage design where writes are append-only and reads are served through an in-memory index (`keydir`) that maps each key to its latest on-disk location. In practice, each write appends a record to a `.data` segment and updates the in-memory pointer, while each read does an O(1) keydir lookup followed by a single seek/read from disk with CRC validation. Old versions and tombstoned keys are cleaned up by merge compaction, and optional `.hint` files accelerate startup by rebuilding keydir without scanning full data files. It is important here because it gives predictable write I/O patterns and low read latency, which makes storage behavior easier to reason about under Raft replication load.

## Raft Primer

Raft is a consensus algorithm that keeps replicas in a consistent state by electing a single leader and requiring quorum agreement for committed writes. Clients submit writes to the leader, the leader appends operations to its replicated log and sends them out to followers, followers acknowledge replication, and entries are considered committed only after a majority confirms them; committed entries are then applied to the state machine in log order on every node. This matters because it provides strong consistency and fault tolerance for distributed writes: as long as a majority of nodes is available, the cluster can continue safely, avoid split-brain write acceptance, and recover leadership automatically after node failures.

## Architecture

- Consensus: Raft leader election and log replication over gRPC.
- Storage: Bitcask v2 engine with append-only `.data` segments, `.hint` files, CRC validation, tombstones, merge compaction, and single-writer locking.
- API: gRPC `KVService` (`Get`, `Put`, `Delete`) for clients and gRPC Raft RPCs (`RequestVote`, `AppendEntries`) for inter-node communication.
- Execution model: writes are acknowledged after quorum replication and state-machine apply; linearizable reads are served by the leader after a replicated read barrier commits, then read from the local Bitcask keydir.

## Releases

Tagged releases publish cross-platform binaries via GitHub Releases:
- `caskv-server` for running cluster nodes
- `caskv-client` for interactive `Get`/`Put`/`Delete`

Version check:
```bash
./caskv-server -version
./caskv-client -version
```

## Benchmark Methodology

Benchmarks are run by `cmd/kv-bench`, which launches an isolated 3-node cluster and captures machine-generated JSON output.

The snapshot below reflects a local run on March 10, 2026 using the flags shown here.

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
  -workdir benchmark-artifacts/$(date +%F)/etcd-compare
```

### What each benchmark measures

- Raft write latency benchmark:
  - Sequential leader-targeted writes.
  - Reports p50/p95/p99 and checks replication convergence.
- Heavy-load throughput benchmark:
  - Concurrent write/read workers for sustained load.
  - Reports throughput, mean latency, tails, and slowest request.
- Light-load latency benchmark:
  - Sequential low-contention `Put`/`Get` latency.
- Disk latency SLO proxy benchmark:
  - `SyncOnPut` latencies as a WAL fsync proxy.
  - Explicit `Sync()` latencies as a backend commit proxy.
- Storage recovery/compaction benchmark:
  - Restart/open times across no-hint vs hint paths.
  - Disk reduction after merge compaction.

### Core project checks

- Raft write p99 latency: **0.72 ms** (target `<10 ms`)
- Restart time (merged+hints median): **40.55 ms**
- Disk reduction after merge: **50.17%** (slightly above the configured 50% upper bound)

### etcd-style workload comparison 

| Scenario | etcd reference target | Raft-KV measured | Result |
|---|---:|---:|---|
| Heavy write throughput (leader-targeted) | 44,000 req/s | 37,871 req/s | miss |
| Heavy write avg latency (leader-targeted) | 22 ms | 1.69 ms | pass |
| Heavy write throughput (all-members target) | 50,000 req/s | 24,206 req/s | miss |
| Heavy write avg latency (all-members target) | 20 ms | 2.64 ms | pass |
| Heavy read throughput (linearizable) | 141,000 req/s | 41,212 req/s | miss |
| Heavy read avg latency (linearizable) | 5.5 ms | 2.33 ms | pass |
| Heavy read throughput (serializable) | 186,000 req/s | 5,896 req/s | miss |
| Heavy read avg latency (serializable) | 2.2 ms | 16.27 ms | miss |
| Light-load `Put` avg latency | <1 ms | 0.349 ms | pass |
| Light-load `Get` avg latency | <1 ms | 0.340 ms | pass |
| WAL fsync p99 proxy | <10 ms | 1.65 ms | pass |
| Backend commit p99 proxy | <25 ms | 1.55 ms | pass |

`etcdctl check perf` style gates:
- `small`: pass
- `medium`: pass
- `large`: fail
- `xlarge`: fail

## Interpreting the Numbers

- The current implementation is **latency-strong** on leader-targeted writes and leader-served linearizable reads, and it meets the local fsync durability SLO proxies.
- The system remains below etcd's published heavy-load throughput references on this hardware and topology.
- The remaining `check perf` failures in this run are driven by high slowest-request outliers at larger worker counts, which points to tail-latency spikes under stress rather than weak average latency.

## Comparison Notes vs etcd

The etcd targets come from published etcd operational/performance references:
- [etcd performance guide](https://etcd.io/docs/v3.6/op-guide/performance/)
- [etcd FAQ performance guidance](https://etcd.io/docs/v3.7/faq/)

Important caveats for fair comparison:
- This project is a focused educational/portfolio implementation, not a feature-complete etcd replacement.
- Linearizable reads in this implementation are leader-only and ordered by a replicated read barrier rather than etcd's ReadIndex path.
- The current "serializable" benchmark name is historical: with follower reads now rejecting and clients retrying through the leader, this path is no longer measuring follower-local stale reads.
- Results are environment-sensitive (CPU, disk class, kernel, network stack, background load).

## Repository Layout

```text
cmd/
  kv-server/   # server binary
  kv-client/   # interactive client
  kv-test/     # integration smoke runner
  kv-bench/    # automated benchmark runner
kvstore/
  store.go     # Bitcask v2 storage engine
raft/
  node.go      # Raft core
server/
  server.go    # Raft + KV service integration
proto/
  raft.proto   # gRPC contracts
```
