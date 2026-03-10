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

## Quick Start

Prerequisites:
- Go `1.24.x` or newer toolchain support
- three terminals for a local cluster

Build local binaries:

```bash
go build -o kv-server ./cmd/kv-server
go build -o kv-client ./cmd/kv-client
```

Start a local 3-node cluster:

Terminal 1:
```bash
./kv-server --id 0 --port 6000 --client-port 8000 \
  --peers localhost:6000,localhost:6001,localhost:6002
```

Terminal 2:
```bash
./kv-server --id 1 --port 6001 --client-port 8001 \
  --peers localhost:6000,localhost:6001,localhost:6002
```

Terminal 3:
```bash
./kv-server --id 2 --port 6002 --client-port 8002 \
  --peers localhost:6000,localhost:6001,localhost:6002
```

Talk to the cluster:

```bash
./kv-client --address localhost:8000
```

If you connect to a follower, the client request will fail with `not leader` and include a leader hint such as `localhost:8002`.

Run the integration smoke test against the default local ports:

```bash
go run ./cmd/kv-test
```

Local node data is written to `kvstore_<id>/` by default. For a clean local restart, remove those directories after stopping the servers.

## Semantics And Limitations

Semantics:
- `Put` and `Delete` are leader-only write operations.
- `Get` is a leader-only linearizable read. The leader first commits a replicated read barrier, then serves the read from the local Bitcask store.
- Followers reject client `Get`, `Put`, and `Delete` requests with `not leader`.
- Rejected follower requests may include a leader redirect hint in `KVResponse.leader`.

Current limitations:
- the external KV API only supports single-key `Get`, `Put`, and `Delete`
- there is no scan, transaction, watch, or compare-and-swap API
- Raft state is not persisted across restart today: term, vote, and replicated log are initialized in memory on node start
- the Bitcask state machine is persisted on disk, but Raft recovery is not yet durable in the same way
- there is no Raft snapshotting or log compaction layer yet
- networking is plaintext gRPC with no authentication or TLS

The restart and storage benchmarks therefore measure the Bitcask storage layer directly, not full durable Raft recovery semantics.

## Build And Test

The repository `go.mod` targets Go `1.24.0` and includes a `toolchain go1.24.4` directive.

Common development commands:

```bash
go test ./...
go build -o kv-server ./cmd/kv-server
go build -o kv-client ./cmd/kv-client
go build -o kv-bench ./cmd/kv-bench
```

Smoke test flow:
- start the local 3-node cluster on `localhost:8000-8002`
- run `go run ./cmd/kv-test`

Benchmark flow:
- native raft/storage benchmark: `go run ./cmd/kv-bench ...`
- mixed-workload benchmark: use the YCSB wrapper under [`bench/ycsb/`](bench/ycsb/)

## Benchmark Methodology

- leader-targeted write latency with leader read-after-write verification
- leader visibility checks for recently written keys
- storage restart/open time across no-hint, hint, and merged-hint datasets
- disk reduction after merge compaction

Example run:

```bash
go build -o kv-server ./cmd/kv-server
go run ./cmd/kv-bench \
  -server-bin ./kv-server \
  -writes 600 \
  -latency-payload-bytes 64 \
  -consistency-sample 60 \
  -dataset-keys 12000 \
  -dataset-rounds 2 \
  -dataset-payload-bytes 512 \
  -restart-trials 3 \
  -keep-artifacts \
  -workdir benchmark-artifacts/$(date +%F)/native-bench
```

### Native Benchmark Snapshot

The snapshot below reflects a local run on March 10, 2026.

- Raft write p99 latency: **0.72 ms** (target `<10 ms`)
- Restart time (merged+hints median): **40.55 ms**
- Disk reduction after merge: **50.17%**

### YCSB Methodology

The local YCSB sweep below was run on March 10, 2026 on a single Linux machine with `8` CPUs and `7.5 GiB` RAM.

Configuration:
- cluster: `3` local `kvraft` nodes
- consistency: leader-targeted linearizable reads and writes
- record model: `fieldcount=1`
- record counts: `recordcount=100000`, `operationcount=200000`
- load phase: `16` client threads
- run phase: thread sweep across `8`, `16`, `32`, and `64`
- value sizes: `fieldlength=256` and `fieldlength=1024`
- workloads:
  - `B`: `95%` read, `5%` update
  - `C`: `100%` read
  - `A`: `50%` read, `50%` update
  - `F`: `50%` read, `50%` read-modify-write

Methodology details:
- each `(fieldlength, threads)` point used a fresh 3-node cluster
- each point loaded a fresh dataset before running workloads
- the YCSB binding targeted the current leader directly
- `F` is the most expensive workload here because YCSB read-modify-write becomes `Get` plus `Put` against a linearizable store

### 32 Vs 64 Threads

`32` threads is the recommended default comparison point for this repository. `64` threads is useful as a saturation check.

#### `fieldlength=256`

| Workload | 32-thread throughput | 64-thread throughput | 32-thread p99 | 64-thread p99 |
|---|---:|---:|---:|---:|
| `B` | `15485.87 ops/s` | `15710.92 ops/s` | read `9.46 ms`, update `18.21 ms` | read `17.36 ms`, update `32.80 ms` |
| `C` | `16607.16 ops/s` | `16668.06 ops/s` | read `9.46 ms` | read `18.00 ms` |
| `A` | `10621.91 ops/s` | `11237.22 ops/s` | read `8.41 ms`, update `17.15 ms` | read `15.25 ms`, update `30.94 ms` |
| `F` | `8756.18 ops/s` | `9550.19 ops/s` | read `7.31 ms`, RMW `21.70 ms` | read `13.52 ms`, RMW `41.34 ms` |

#### `fieldlength=1024`

| Workload | 32-thread throughput | 64-thread throughput | 32-thread p99 | 64-thread p99 |
|---|---:|---:|---:|---:|
| `B` | `14191.44 ops/s` | `13547.38 ops/s` | read `10.02 ms`, update `20.34 ms` | read `18.98 ms`, update `40.03 ms` |
| `C` | `15405.95 ops/s` | `16025.64 ops/s` | read `9.97 ms` | read `17.70 ms` |
| `A` | `9833.33 ops/s` | `10162.09 ops/s` | read `9.19 ms`, update `19.65 ms` | read `17.66 ms`, update `36.93 ms` |
| `F` | `7594.46 ops/s` | `7992.33 ops/s` | read `7.01 ms`, RMW `21.15 ms` | read `15.13 ms`, RMW `48.16 ms` |

### Result Interpretation

The main pattern is that `64` threads usually provides only a small throughput gain over `32`, while tail latency grows sharply.

What the sweep shows:
- `C` continues to scale slightly at `64`, but the gain over `32` is tiny compared with the near-doubling in read p99.
- `A` and `F` gain some additional throughput at `64`, but update and read-modify-write p99 latencies become much worse.
- `B` saturates earlier; at `1024` bytes it is already slower at `64` than at `32`.
- Larger values hurt write-heavy paths more than read-only paths, which is expected because bigger values travel through replication, storage append, and state-machine apply.

Why `32` threads is the better baseline:
- it is near the throughput knee on this hardware
- it avoids the large p99 penalty seen at `64`
- it gives a better balance across all workloads instead of optimizing only for peak read throughput

## Repository Layout

```text
cmd/
  kv-server/   # server binary
  kv-client/   # interactive client
  kv-test/     # integration smoke runner
  kv-bench/    # native raft/storage benchmark runner
bench/
  ycsb/        # YCSB workloads and runner wrapper
kvstore/
  store.go     # Bitcask v2 storage engine
raft/
  node.go      # Raft core
server/
  server.go    # Raft + KV service integration
proto/
  raft.proto   # gRPC contracts
```
