#!/bin/bash

# Simple script to run the Raft-KV cluster with gRPC

echo "========================================="
echo "Raft-KV Cluster Setup"
echo "========================================="
echo ""

# Check if protoc is installed
if ! command -v protoc &> /dev/null; then
    echo "⚠ Warning: protoc not found. Please install Protocol Buffers compiler."
    echo "  macOS: brew install protobuf"
    echo "  Linux: apt-get install -y protobuf-compiler"
    echo ""
fi

echo "Step 2: Downloading Go dependencies..."
go mod download
if [ $? -ne 0 ]; then
    echo "✗ Failed to download dependencies"
    exit 1
fi
echo ""

echo "Step 3: Building server..."
go build -o raft-kv ./cmd/kv-server
if [ $? -ne 0 ]; then
    echo "✗ Failed to build server"
    exit 1
fi
echo "✓ Server built successfully"
echo ""

echo "Step 4: Building client..."
go build -o client ./cmd/kv-client
if [ $? -ne 0 ]; then
    echo "✗ Failed to build client"
    exit 1
fi
echo "✓ Client built successfully"
echo ""

echo "Step 5: Starting 3-node Raft cluster..."
echo ""

# Function to check if a port is available
is_port_available() {
    local port=$1
    ! lsof -i :$port >/dev/null 2>&1
}

# Function to find an available port starting from a base port
find_available_port() {
    local base_port=$1
    local port=$base_port
    while ! is_port_available $port; do
        port=$((port + 1))
        if [ $port -gt $((base_port + 100)) ]; then
            echo "Error: Could not find available port in range $base_port-$((base_port + 100))"
            exit 1
        fi
    done
    echo $port
}

# Find available ports for each node
echo "Scanning for available ports..."

# Raft RPC ports (starting from 5000)
RAFT_PORT_0=$(find_available_port 5000)
RAFT_PORT_1=$(find_available_port $((RAFT_PORT_0 + 1)))
RAFT_PORT_2=$(find_available_port $((RAFT_PORT_1 + 1)))

# Client API ports (starting from 8000)
CLIENT_PORT_0=$(find_available_port 8000)
CLIENT_PORT_1=$(find_available_port $((CLIENT_PORT_0 + 1)))
CLIENT_PORT_2=$(find_available_port $((CLIENT_PORT_1 + 1)))

echo "✓ Found available ports"
echo ""
echo "Node Configuration:"
echo "  Node 0: Raft RPC on localhost:$RAFT_PORT_0, Client API on localhost:$CLIENT_PORT_0"
echo "  Node 1: Raft RPC on localhost:$RAFT_PORT_1, Client API on localhost:$CLIENT_PORT_1"
echo "  Node 2: Raft RPC on localhost:$RAFT_PORT_2, Client API on localhost:$CLIENT_PORT_2"
echo ""
echo "To connect with client: ./client -address=localhost:$CLIENT_PORT_0"
echo "To run tests: go run ./cmd/kv-test"
echo ""
echo "Press Ctrl+C to stop all nodes"
echo ""
echo "========================================="
echo ""

# Build peer lists for each node
PEERS_0="localhost:$RAFT_PORT_1,localhost:$RAFT_PORT_2"
PEERS_1="localhost:$RAFT_PORT_0,localhost:$RAFT_PORT_2"
PEERS_2="localhost:$RAFT_PORT_0,localhost:$RAFT_PORT_1"

# Start nodes in background
./raft-kv -id=0 -port=$RAFT_PORT_0 -client-port=$CLIENT_PORT_0 -peers=$PEERS_0 &
PID1=$!
echo "Started Node 0 (PID: $PID1)"

./raft-kv -id=1 -port=$RAFT_PORT_1 -client-port=$CLIENT_PORT_1 -peers=$PEERS_1 &
PID2=$!
echo "Started Node 1 (PID: $PID2)"

./raft-kv -id=2 -port=$RAFT_PORT_2 -client-port=$CLIENT_PORT_2 -peers=$PEERS_2 &
PID3=$!
echo "Started Node 2 (PID: $PID3)"

echo ""
echo "✓ All nodes started!"
echo ""

# Function to kill all processes on exit
cleanup() {
    echo ""
    echo "========================================="
    echo "Shutting down cluster..."
    echo "========================================="
    kill $PID1 $PID2 $PID3 2>/dev/null
    wait $PID1 $PID2 $PID3 2>/dev/null
    echo "✓ All nodes stopped"
    exit 0
}

trap cleanup SIGINT SIGTERM

# Wait for all background processes
wait