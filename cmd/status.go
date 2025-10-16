// cmd/status.go
/*
Copyright © 2025 Jason Sun <jason@aceteam.ai>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/spf13/cobra"
)

// TailscaleStatus represents the relevant fields from `tailscale status --json`
type TailscaleStatus struct {
	Self struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Self"`
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Shows a comprehensive status of the Citadel node",
	Long: `Provides a full health check and resource overview of the Citadel node.
It checks network connectivity, system vitals (CPU, RAM, Disk), GPU status,
and the state of all managed services.`,
	Run: func(cmd *cobra.Command, args []string) {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		defer w.Flush()

		fmt.Fprintln(w, "--- 📊 Citadel Node Status ---")

		// --- 1. System Vitals ---
		fmt.Fprintln(w, "\n💻 SYSTEM VITALS\t")
		printMemInfo(w)
		printCPUInfo(w)
		printDiskInfo(w)

		// --- 2. GPU Status ---
		fmt.Fprintln(w, "\n💎 GPU STATUS\t")
		printGPUInfo(w)

		// --- 3. Network Status ---
		fmt.Fprintln(w, "\n🌐 NETWORK STATUS\t")
		printNetworkInfo(w)

		// --- 4. Service Status ---
		fmt.Fprintln(w, "\n🚀 MANAGED SERVICES\t")
		printServiceInfo(w)
	},
}

func printMemInfo(w *tabwriter.Writer) {
	v, err := mem.VirtualMemory()
	if err != nil {
		fmt.Fprintf(w, "  Memory:\tError getting memory info: %v\n", err)
		return
	}
	fmt.Fprintf(w, "  Memory:\t%.1f%% (%s / %s)\n", v.UsedPercent, formatBytes(v.Used), formatBytes(v.Total))
}

func printCPUInfo(w *tabwriter.Writer) {
	percentages, err := cpu.Percent(time.Second, false)
	if err != nil || len(percentages) == 0 {
		fmt.Fprintf(w, "  CPU Usage:\tError getting CPU info: %v\n", err)
		return
	}
	fmt.Fprintf(w, "  CPU Usage:\t%.1f%%\n", percentages[0])
}

func printDiskInfo(w *tabwriter.Writer) {
	d, err := disk.Usage("/")
	if err != nil {
		fmt.Fprintf(w, "  Disk (/):\tError getting disk info: %v\n", err)
		return
	}
	fmt.Fprintf(w, "  Disk (/):\t%.1f%% (%s / %s)\n", d.UsedPercent, formatBytes(d.Used), formatBytes(d.Total))
}

func printGPUInfo(w *tabwriter.Writer) {
	// nvidia-smi is the source of truth. Query it for specific fields.
	cmd := exec.Command("nvidia-smi", "--query-gpu=name,temperature.gpu,power.draw,memory.used,memory.total,utilization.gpu", "--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(w, "  NVIDIA GPU:\tNot detected or nvidia-smi not found.\t")
		return
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for i, line := range lines {
		parts := strings.Split(line, ", ")
		if len(parts) < 6 {
			continue
		}
		gpuName := parts[0]
		temp := parts[1]
		power := parts[2]
		memUsed := parts[3]
		memTotal := parts[4]
		util := parts[5]

		fmt.Fprintf(w, "  GPU %d:\t%s\n", i, gpuName)
		fmt.Fprintf(w, "  \tTemp: %s°C\tPower: %sW\tUtil: %s%%\n", temp, power, util)
		fmt.Fprintf(w, "  \tMemory:\t%sMiB / %sMiB\n", memUsed, memTotal)
	}
}

func printNetworkInfo(w *tabwriter.Writer) {
	tsCmd := exec.Command("tailscale", "status", "--json")
	output, err := tsCmd.Output()
	if err != nil {
		fmt.Fprintln(w, "  Connection:\t🔴 OFFLINE (Tailscale daemon not responding)\t")
		return
	}

	var status TailscaleStatus
	if err := json.Unmarshal(output, &status); err != nil {
		fmt.Fprintln(w, "  Connection:\t⚠️  WARNING (Could not parse Tailscale status)\t")
		return
	}

	if status.Self.Online {
		fmt.Fprintln(w, "  Connection:\t🟢 ONLINE to Nexus\t")
		fmt.Fprintf(w, "  Hostname:\t%s\n", strings.TrimSuffix(status.Self.DNSName, "."))
		fmt.Fprintf(w, "  IP Address:\t%s\n", status.Self.TailscaleIPs[0])
	} else {
		fmt.Fprintln(w, "  Connection:\t🔴 OFFLINE (Not connected to Nexus)\t")
	}
}

func printServiceInfo(w *tabwriter.Writer) {
	manifest, err := readManifest("citadel.yaml")
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "  (No citadel.yaml found, no services to check)\t\t")
		} else {
			fmt.Fprintf(w, "  Error reading manifest: %v\n", err)
		}
		return
	}

	if len(manifest.Services) == 0 {
		fmt.Fprintln(w, "  (Manifest contains no services)\t\t")
		return
	}

	for _, service := range manifest.Services {
		psCmd := exec.Command("docker", "compose", "-f", service.ComposeFile, "ps", "--format", "json")
		output, err := psCmd.Output()
		if err != nil {
			fmt.Fprintf(w, "  - %s:\t⚠️  Could not get status\n", service.Name)
			continue
		}

		var containers []struct {
			Name   string `json:"Name"`
			State  string `json:"State"`
			Health string `json:"Health"`
			Image  string `json:"Image"`
		}

		// Docker compose ps --format json returns a stream of json objects, not an array
		decoder := json.NewDecoder(strings.NewReader(string(output)))
		for decoder.More() {
			var c struct {
				Name   string `json:"Name"`
				State  string `json:"State"`
				Health string `json:"Health"`
				Image  string `json:"Image"`
			}
			if err := decoder.Decode(&c); err != nil {
				continue
			}
			containers = append(containers, c)
		}

		if len(containers) == 0 {
			fmt.Fprintf(w, "  - %s:\t⚫ STOPPED\n", service.Name)
		} else {
			// For simplicity, report status of the first container for the service
			state := strings.ToUpper(containers[0].State)
			statusIcon := "⚫" // Default to stopped/unknown
			if strings.Contains(state, "RUNNING") || strings.Contains(state, "UP") {
				statusIcon = "🟢"
			} else if strings.Contains(state, "EXITED") || strings.Contains(state, "DEAD") {
				statusIcon = "🔴"
			}
			fmt.Fprintf(w, "  - %s:\t%s %s\n", service.Name, statusIcon, state)
		}
	}
}

// formatBytes is a helper to convert bytes to a human-readable string.
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
