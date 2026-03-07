package cluster

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type NodeProcess struct {
	id         int
	clientAddr string
	cmd        *exec.Cmd
	logFile    *os.File
}

func (n *NodeProcess) stop() error {
	if n == nil || n.cmd == nil || n.cmd.Process == nil {
		if n != nil && n.logFile != nil {
			return n.logFile.Close()
		}
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- n.cmd.Wait()
	}()

	_ = n.cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = n.cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	if n.logFile != nil {
		if err := n.logFile.Close(); err != nil {
			return err
		}
	}
	return nil
}

type Cluster struct {
	nodes       []*NodeProcess
	ClientAddrs []string
}

func (c *Cluster) StopAll() {
	for _, node := range c.nodes {
		if err := node.stop(); err != nil {
			log.Printf("warning: failed stopping node %d: %v", node.id, err)
		}
	}
}

func Start(serverBin, runDir string, n int) (*Cluster, error) {
	raftPorts, err := reserveFreePorts(n)
	if err != nil {
		return nil, err
	}
	clientPorts, err := reserveFreePorts(n)
	if err != nil {
		return nil, err
	}

	raftAddrs := make([]string, 0, n)
	for _, p := range raftPorts {
		raftAddrs = append(raftAddrs, "127.0.0.1:"+strconv.Itoa(p))
	}
	clientAddrs := make([]string, 0, n)
	for _, p := range clientPorts {
		clientAddrs = append(clientAddrs, "127.0.0.1:"+strconv.Itoa(p))
	}

	peersFlag := strings.Join(raftAddrs, ",")

	nodes := make([]*NodeProcess, 0, n)
	for i := 0; i < n; i++ {
		logPath := filepath.Join(runDir, fmt.Sprintf("node%d.log", i))
		lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			for _, node := range nodes {
				_ = node.stop()
			}
			return nil, fmt.Errorf("open log file for node %d: %w", i, err)
		}

		cmd := exec.Command(
			serverBin,
			"-id="+strconv.Itoa(i),
			"-port="+strconv.Itoa(raftPorts[i]),
			"-client-port="+strconv.Itoa(clientPorts[i]),
			"-peers="+peersFlag,
		)
		cmd.Dir = runDir
		cmd.Stdout = lf
		cmd.Stderr = lf

		if err := cmd.Start(); err != nil {
			_ = lf.Close()
			for _, node := range nodes {
				_ = node.stop()
			}
			return nil, fmt.Errorf("start node %d: %w", i, err)
		}

		nodes = append(nodes, &NodeProcess{id: i, clientAddr: clientAddrs[i], cmd: cmd, logFile: lf})
	}

	return &Cluster{nodes: nodes, ClientAddrs: clientAddrs}, nil
}

func reserveFreePorts(n int) ([]int, error) {
	ports := make([]int, 0, n)
	listeners := make([]net.Listener, 0, n)
	seen := make(map[int]struct{})

	for len(ports) < n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return nil, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if _, ok := seen[port]; ok {
			_ = ln.Close()
			continue
		}
		seen[port] = struct{}{}
		listeners = append(listeners, ln)
		ports = append(ports, port)
	}

	for _, ln := range listeners {
		_ = ln.Close()
	}
	return ports, nil
}
