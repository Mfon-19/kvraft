#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
MODE=${1:-both}
WORKLOAD_NAME=${2:-workloadb}
shift $(( $# > 0 ? 1 : 0 )) || true
shift $(( $# > 0 ? 1 : 0 )) || true
EXTRA_ARGS=("$@")

if [[ -z "${YCSB_HOME:-}" ]]; then
  echo "YCSB_HOME must point to a YCSB checkout with the kvraft binding" >&2
  exit 1
fi
if [[ -z "${KVRAFT_TARGET:-}" ]]; then
  echo "KVRAFT_TARGET must point to a kvraft client address" >&2
  exit 1
fi

case "${WORKLOAD_NAME,,}" in
  a|workloada)
    WORKLOAD_FILE="$SCRIPT_DIR/workloada.properties"
    ;;
  b|workloadb)
    WORKLOAD_FILE="$SCRIPT_DIR/workloadb.properties"
    ;;
  c|workloadc)
    WORKLOAD_FILE="$SCRIPT_DIR/workloadc.properties"
    ;;
  f|workloadf)
    WORKLOAD_FILE="$SCRIPT_DIR/workloadf.properties"
    ;;
  *)
    echo "unknown workload: $WORKLOAD_NAME" >&2
    exit 1
    ;;
esac

LOAD_THREADS=${LOAD_THREADS:-${THREADS:-16}}
RUN_THREADS=${RUN_THREADS:-${THREADS:-32}}
RECORDCOUNT=${RECORDCOUNT:-100000}
OPERATIONCOUNT=${OPERATIONCOUNT:-100000}
FIELDCOUNT=${FIELDCOUNT:-1}
FIELDLENGTH=${FIELDLENGTH:-256}
TABLE=${TABLE:-usertable}
KVRAFT_RPC_TIMEOUT_MS=${KVRAFT_RPC_TIMEOUT_MS:-5000}
KVRAFT_MAX_REDIRECTS=${KVRAFT_MAX_REDIRECTS:-3}

COMMON_ARGS=(
  kvraft
  -P "$WORKLOAD_FILE"
  -p "table=$TABLE"
  -p "recordcount=$RECORDCOUNT"
  -p "operationcount=$OPERATIONCOUNT"
  -p "fieldcount=$FIELDCOUNT"
  -p "fieldlength=$FIELDLENGTH"
  -p "kvraft.target=$KVRAFT_TARGET"
  -p "kvraft.rpc_timeout_ms=$KVRAFT_RPC_TIMEOUT_MS"
  -p "kvraft.max_redirects=$KVRAFT_MAX_REDIRECTS"
  -s
)

if [[ -n "${KVRAFT_TABLE_PREFIX:-}" ]]; then
  COMMON_ARGS+=( -p "kvraft.tableprefix=$KVRAFT_TABLE_PREFIX" )
fi

run_ycsb() {
  local phase=$1
  shift
  echo ">>> ycsb.sh $phase ${COMMON_ARGS[*]} $*"
  "$YCSB_HOME/bin/ycsb.sh" "$phase" "${COMMON_ARGS[@]}" "$@"
}

case "$MODE" in
  load)
    run_ycsb load -threads "$LOAD_THREADS" "${EXTRA_ARGS[@]}"
    ;;
  run)
    run_ycsb run -threads "$RUN_THREADS" "${EXTRA_ARGS[@]}"
    ;;
  both)
    run_ycsb load -threads "$LOAD_THREADS" "${EXTRA_ARGS[@]}"
    run_ycsb run -threads "$RUN_THREADS" "${EXTRA_ARGS[@]}"
    ;;
  *)
    echo "usage: $0 [load|run|both] [workloada|workloadb|workloadc|workloadf] [extra ycsb args...]" >&2
    exit 1
    ;;
esac
