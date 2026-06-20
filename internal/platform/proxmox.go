package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const proxmoxDetectionTimeout = 5 * time.Second

// ProxmoxInfo holds the result of Proxmox hypervisor detection.
type ProxmoxInfo struct {
	IsInstalled bool   `json:"is_installed"`
	Version     string `json:"version,omitempty"`     // e.g. "pve-manager/8.2.4/..."
	NodeName    string `json:"node_name,omitempty"`    // this PVE node's name
	NodeCount   int    `json:"node_count,omitempty"`   // total nodes in cluster
	VMCount     int    `json:"vm_count,omitempty"`     // QEMU VMs on this node
	CTCount     int    `json:"ct_count,omitempty"`     // LXC containers on this node
}

// DetectProxmox checks whether the host is a Proxmox VE node.
// Detection is split into two phases:
//  1. Presence check (/etc/pve + pvesh on PATH) -- cheap, no root needed.
//  2. Enumeration (pveversion, pvesh get) -- needs root, best-effort with timeout.
//
// On non-Proxmox systems, returns &ProxmoxInfo{IsInstalled: false}, nil.
func DetectProxmox() (*ProxmoxInfo, error) {
	// Phase 1: fast presence check
	if _, err := os.Stat("/etc/pve"); os.IsNotExist(err) {
		return &ProxmoxInfo{IsInstalled: false}, nil
	}
	if _, err := exec.LookPath("pvesh"); err != nil {
		return &ProxmoxInfo{IsInstalled: false}, nil
	}

	info := &ProxmoxInfo{IsInstalled: true}

	// Phase 2: best-effort enumeration (each call gets its own timeout)
	ctx, cancel := context.WithTimeout(context.Background(), proxmoxDetectionTimeout)
	defer cancel()

	// Get version
	info.Version = detectProxmoxVersion(ctx)

	// Get node list (gives us NodeName and NodeCount)
	info.NodeName, info.NodeCount = detectProxmoxNodes(ctx)

	// Get VM and CT counts (requires NodeName)
	if info.NodeName != "" {
		info.VMCount = countProxmoxGuests(ctx, info.NodeName, "qemu")
		info.CTCount = countProxmoxGuests(ctx, info.NodeName, "lxc")
	}

	return info, nil
}

// detectProxmoxVersion runs pveversion and extracts the version string.
func detectProxmoxVersion(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "pveversion")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// detectProxmoxNodes calls pvesh to list cluster nodes. Returns the local
// node name (hostname match) and total node count.
func detectProxmoxNodes(ctx context.Context) (nodeName string, nodeCount int) {
	cmd := exec.CommandContext(ctx, "pvesh", "get", "/nodes", "--output-format", "json")
	output, err := cmd.Output()
	if err != nil {
		// Fallback: use hostname as node name
		if hn, err := os.Hostname(); err == nil {
			return hn, 1
		}
		return "", 0
	}

	nodes, err := parseProxmoxNodeList(output)
	if err != nil || len(nodes) == 0 {
		if hn, err := os.Hostname(); err == nil {
			return hn, 1
		}
		return "", 0
	}

	nodeCount = len(nodes)

	// Find the local node (status "online" is typical, but just take the first
	// if there's only one, or match hostname).
	hostname, _ := os.Hostname()
	for _, n := range nodes {
		if n == hostname {
			return n, nodeCount
		}
	}

	// If hostname didn't match, return the first node
	return nodes[0], nodeCount
}

// parseProxmoxNodeList extracts node names from pvesh JSON output.
// Exported for testing with fixture data.
func parseProxmoxNodeList(data []byte) ([]string, error) {
	var nodes []struct {
		Node string `json:"node"`
	}
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, fmt.Errorf("failed to parse node list: %w", err)
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.Node != "" {
			names = append(names, n.Node)
		}
	}
	return names, nil
}

// countProxmoxGuests calls pvesh to count QEMU VMs or LXC containers.
func countProxmoxGuests(ctx context.Context, nodeName, guestType string) int {
	path := fmt.Sprintf("/nodes/%s/%s", nodeName, guestType)
	cmd := exec.CommandContext(ctx, "pvesh", "get", path, "--output-format", "json")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	count, _ := parseProxmoxGuestCount(output)
	return count
}

// parseProxmoxGuestCount counts entries in a pvesh guest list JSON array.
// Exported for testing with fixture data.
func parseProxmoxGuestCount(data []byte) (int, error) {
	var guests []json.RawMessage
	if err := json.Unmarshal(data, &guests); err != nil {
		return 0, fmt.Errorf("failed to parse guest list: %w", err)
	}
	return len(guests), nil
}
