# YCSB Benchmarks

This directory contains the reproducible YCSB workload contract for `kvraft`.

## Prerequisites

- a YCSB checkout that includes the `kvraft` binding module
- Java and Maven available to build that binding
- a running `kvraft` cluster

Build the binding in your YCSB checkout:

```bash
cd "$YCSB_HOME"
mvn -pl site.ycsb:kvraft-binding -am clean package
```

## Supported benchmark contract

- `fieldcount=1`
- `scanproportion=0`
- reads are leader-only linearizable reads
- the binding follows `KVResponse.leader` hints, so `KVRAFT_TARGET` may point at any member
- `update` and workload `F` use client-side read-modify-write because `kvraft` exposes `Get`/`Put`/`Delete`, not partial record updates

## Workloads

- `workloada.properties`: 50/50 read/update
- `workloadb.properties`: 95/5 read/update
- `workloadc.properties`: 100% read
- `workloadf.properties`: 50/50 read/read-modify-write

## Run

```bash
export YCSB_HOME=/path/to/YCSB
export KVRAFT_TARGET=localhost:8000

bench/ycsb/run.sh load workloadb
bench/ycsb/run.sh run workloadb
```

Run load and run in one command:

```bash
bench/ycsb/run.sh both workloadb
```

Common overrides:

```bash
THREADS=32 RECORDCOUNT=100000 OPERATIONCOUNT=200000 FIELDLENGTH=256 \
  bench/ycsb/run.sh both workloadb
```

You can also pass extra YCSB flags after the workload name.
