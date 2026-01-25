// cmd/controlcenter.go
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

// runControlCenter launches the unified control center TUI
func runControlCenter() {
	if !tui.IsTTY() {
		fmt.Fprintln(os.Stderr, "Control center requires a terminal. Use --daemon for background mode.")
		os.Exit(1)
	}

	cfg := controlcenter.Config{
		Version:        Version,
		RefreshFn:      gatherControlCenterData,
		StartServiceFn: ccStartService,
		StopServiceFn:  ccStopService,
	}

	cc := controlcenter.New(cfg)

	// Start the UI immediately, load data in background
	go func() {
		if data, err := gatherControlCenterData(); err == nil {
			cc.UpdateData(data)
		}
	}()

	if err := cc.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Control center error: %v\n", err)
		os.Exit(1)
	}
}

// gatherControlCenterData collects all data for the control center
func gatherControlCenterData() (controlcenter.StatusData, error) {
	data := controlcenter.StatusData{
		Version: Version,
	}

	// Load manifest
	manifest, configDir, _ := findAndReadManifest()
	if manifest != nil {
		data.NodeName = manifest.Node.Name
		data.OrgID = manifest.Node.OrgID
	}

	// Get hostname if not in manifest
	if data.NodeName == "" {
		data.NodeName, _ = os.Hostname()
	}

	// System vitals - Memory
	if v, err := mem.VirtualMemory(); err == nil {
		data.MemoryPercent = v.UsedPercent
		data.MemoryUsed = formatBytes(v.Used)
		data.MemoryTotal = formatBytes(v.Total)
	}

	// System vitals - CPU
	if percentages, err := cpu.Percent(200*time.Millisecond, false); err == nil && len(percentages) > 0 {
		data.CPUPercent = percentages[0]
	}

	// System vitals - Disk
	if d, err := disk.Usage("/"); err == nil {
		data.DiskPercent = d.UsedPercent
		data.DiskUsed = formatBytes(d.Used)
		data.DiskTotal = formatBytes(d.Total)
	}

	// GPU info
	if detector, err := platform.GetGPUDetector(); err == nil && detector.HasGPU() {
		if gpus, err := detector.GetGPUInfo(); err == nil && len(gpus) > 0 {
			gpu := gpus[0]
			data.GPUName = gpu.Name
			data.GPUMemory = gpu.Memory
			data.GPUTemp = gpu.Temperature
			if gpu.Utilization != "" {
				utilStr := strings.TrimSuffix(gpu.Utilization, "%")
				if util, err := strconv.ParseFloat(utilStr, 64); err == nil {
					data.GPUUtilization = util
				}
			}
		}
	}

	// Network status - check connection state without blocking
	if network.HasState() {
		// Check if already connected (don't try to reconnect, that blocks)
		if network.IsGlobalConnected() {
			data.Connected = true

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if status, err := network.GetGlobalStatus(ctx); err == nil {
				data.NodeIP = status.IPv4
				if data.NodeName == "" {
					data.NodeName = status.Hostname
				}
			}
			cancel()

			// Get peers (with short timeout)
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			myIP, _ := network.GetGlobalIPv4()
			if peers, err := network.GetGlobalPeers(ctx2); err == nil {
				for _, peer := range peers {
					if peer.IP != myIP {
						peerInfo := controlcenter.PeerInfo{
							Hostname: peer.Hostname,
							IP:       peer.IP,
							Online:   peer.Online,
						}

						// Skip ping for now to avoid blocking
						data.Peers = append(data.Peers, peerInfo)
					}
				}
			}
			cancel2()
		}
	}

	// Services
	if manifest != nil && configDir != "" {
		for _, service := range manifest.Services {
			svcInfo := controlcenter.ServiceInfo{
				Name:   service.Name,
				Status: "stopped",
			}

			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			if _, err := os.Stat(fullComposePath); err == nil {
				psCmd := exec.Command("docker", "compose", "-f", fullComposePath, "ps", "--format", "json")
				if output, err := psCmd.Output(); err == nil {
					var containers []struct {
						State  string `json:"State"`
						Status string `json:"Status"`
					}
					decoder := json.NewDecoder(strings.NewReader(string(output)))
					for decoder.More() {
						var c struct {
							State  string `json:"State"`
							Status string `json:"Status"`
						}
						if err := decoder.Decode(&c); err == nil {
							containers = append(containers, c)
						}
					}
					if len(containers) > 0 {
						state := strings.ToLower(containers[0].State)
						if strings.Contains(state, "running") || strings.Contains(state, "up") {
							svcInfo.Status = "running"
							// Try to extract uptime from Status field
							if containers[0].Status != "" {
								svcInfo.Uptime = extractUptime(containers[0].Status)
							}
						} else if strings.Contains(state, "exited") || strings.Contains(state, "dead") {
							svcInfo.Status = "stopped"
						} else {
							svcInfo.Status = state
						}
					}
				}
			}

			data.Services = append(data.Services, svcInfo)
		}
	}

	return data, nil
}

// extractUptime tries to extract uptime from docker status string like "Up 2 hours"
func extractUptime(status string) string {
	status = strings.ToLower(status)
	if uptime, found := strings.CutPrefix(status, "up "); found {
		return uptime
	}
	return ""
}

// ccStartService starts a service by name
func ccStartService(name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			cmd := exec.Command("docker", "compose", "-f", fullComposePath, "-p", "citadel-"+name, "up", "-d")
			return cmd.Run()
		}
	}

	return fmt.Errorf("service not found: %s", name)
}

// ccStopService stops a service by name
func ccStopService(name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			cmd := exec.Command("docker", "compose", "-f", fullComposePath, "-p", "citadel-"+name, "down")
			return cmd.Run()
		}
	}

	return fmt.Errorf("service not found: %s", name)
}

