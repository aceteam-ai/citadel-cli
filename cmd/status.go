// cmd/status.go
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/spf13/cobra"
)

type TailscaleStatus struct {
	Self struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Self"`
}

var (
	headerColor = color.New(color.FgCyan, color.Bold)
	goodColor   = color.New(color.FgGreen)
	warnColor   = color.New(color.FgYellow)
	badColor    = color.New(color.FgRed)
	labelColor  = color.New(color.Bold)
	noColor     bool // Flag to disable color
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Shows a comprehensive status of the Citadel node",
	Long: `Provides a full health check and resource overview of the Citadel node.
It checks network connectivity, system vitals (CPU, RAM, Disk), GPU status,
and the state of all managed services.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Handle the --no-color flag
		if noColor {
			color.NoColor = true
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		defer w.Flush()

		headerColor.Fprintln(w, "--- 📊 Citadel Node Status ---")

		headerColor.Fprintln(w, "\n💻 SYSTEM VITALS")
		printMemInfo(w)
		printCPUInfo(w)
		printDiskInfo(w)

		headerColor.Fprintln(w, "\n🗂️ CACHE USAGE (~/citadel-cache)")
		printCacheInfo(w)

		headerColor.Fprintln(w, "\n💎 GPU STATUS")
		printGPUInfo(w)

		headerColor.Fprintln(w, "\n🌐 NETWORK STATUS")
		printNetworkInfo(w)

		headerColor.Fprintln(w, "\n🚀 MANAGED SERVICES")
		printServiceInfo(w)
	},
}

// --- printCacheInfo IS NOW FIXED ---
func printCacheInfo(w *tabwriter.Writer) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(w, "  %s\n", badColor.Sprintf("Could not determine home directory: %v", err))
		return
	}
	cacheDir := filepath.Join(homeDir, "citadel-cache")

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		fmt.Fprintf(w, "  (Cache directory not found)\t\n")
		return
	}

	// Get total size (this part was already correct)
	totalCmd := exec.Command("du", "-sh", cacheDir)
	totalOutput, err := totalCmd.Output()
	if err == nil {
		parts := strings.Fields(string(totalOutput))
		if len(parts) > 0 {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Total Size"), parts[0])
		}
	}

	// --- REWRITTEN BREAKDOWN LOGIC ---
	fmt.Fprintf(w, "  %s:\n", labelColor.Sprint("Breakdown"))
	// Use Go's Glob to find all subdirectories/files
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*"))
	if err != nil || len(entries) == 0 {
		fmt.Fprintf(w, "    (Empty)\n")
		return
	}

	// Iterate over the found entries and run `du` on each one
	for _, entry := range entries {
		cmd := exec.Command("du", "-sh", entry)
		output, err := cmd.Output()
		if err != nil {
			continue // Skip if we can't get the size of an entry
		}
		parts := strings.Fields(string(output))
		if len(parts) >= 2 {
			size := parts[0]
			// Use filepath.Base to get just the directory name
			name := filepath.Base(parts[1])
			fmt.Fprintf(w, "    - %s:\t%s\n", name, size)
		}
	}
}

// (The rest of the file remains largely the same, with minor formatting tweaks)
func printMemInfo(w *tabwriter.Writer) {
	v, err := mem.VirtualMemory()
	if err != nil {
		fmt.Fprintf(w, "  🧠 Memory:\t%s\n", badColor.Sprintf("Error getting memory info: %v", err))
		return
	}
	percentStr := colorizePercent(v.UsedPercent)
	fmt.Fprintf(w, "  🧠 %s:\t%s (%s / %s)\n", labelColor.Sprint("Memory"), percentStr, formatBytes(v.Used), formatBytes(v.Total))
}

func printCPUInfo(w *tabwriter.Writer) {
	percentages, err := cpu.Percent(time.Second, false)
	if err != nil || len(percentages) == 0 {
		fmt.Fprintf(w, "  ⚡️ CPU Usage:\t%s\n", badColor.Sprintf("Error getting CPU info: %v", err))
		return
	}
	percentStr := colorizePercent(percentages[0])
	fmt.Fprintf(w, "  ⚡️ %s:\t%s\n", labelColor.Sprint("CPU Usage"), percentStr)
}

func printDiskInfo(w *tabwriter.Writer) {
	d, err := disk.Usage("/")
	if err != nil {
		fmt.Fprintf(w, "  💾 Disk (/):\t%s\n", badColor.Sprintf("Error getting disk info: %v", err))
		return
	}
	percentStr := colorizePercent(d.UsedPercent)
	fmt.Fprintf(w, "  💾 %s:\t%s (%s / %s)\n", labelColor.Sprint("Disk (/)"), percentStr, formatBytes(d.Used), formatBytes(d.Total))
}

func printGPUInfo(w *tabwriter.Writer) {
	cmd := exec.Command("nvidia-smi", "--query-gpu=name,temperature.gpu,power.draw,memory.used,memory.total,utilization.gpu", "--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(w, "  NVIDIA GPU:\tNot detected or nvidia-smi not found.")
		return
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for i, line := range lines {
		parts := strings.Split(line, ", ")
		if len(parts) < 6 {
			continue
		}
		gpuName, temp, power := parts[0], parts[1], parts[2]
		memUsed, _ := strconv.ParseFloat(parts[3], 64)
		memTotal, _ := strconv.ParseFloat(parts[4], 64)
		util, _ := strconv.ParseFloat(parts[5], 64)

		memPercent := 0.0
		if memTotal > 0 {
			memPercent = (memUsed / memTotal) * 100
		}

		fmt.Fprintf(w, "  %s %d:\t%s\n", labelColor.Sprint("GPU"), i, gpuName)
		fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Temp"), colorizeTemp(temp))
		fmt.Fprintf(w, "    - %s:\t%sW\n", labelColor.Sprint("Power"), power)
		fmt.Fprintf(w, "    - %s:\t%s (%sMiB / %sMiB)\n", labelColor.Sprint("VRAM"), colorizePercent(memPercent), parts[3], parts[4])
		fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Util"), colorizePercent(util))
	}
}

func printNetworkInfo(w *tabwriter.Writer) {
	tsCmd := exec.Command("tailscale", "status", "--json")
	output, err := tsCmd.Output()
	if err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Tailscale daemon not responding)"))
		return
	}

	var status TailscaleStatus
	if err := json.Unmarshal(output, &status); err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), warnColor.Sprint("⚠️  WARNING (Could not parse Tailscale status)"))
		return
	}

	if status.Self.Online {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), goodColor.Sprint("🟢 ONLINE to Nexus"))
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Hostname"), strings.TrimSuffix(status.Self.DNSName, "."))
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("IP Address"), status.Self.TailscaleIPs[0])
	} else {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Not connected to Nexus)"))
	}
}

func printServiceInfo(w *tabwriter.Writer) {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// The error from findAndReadManifest is already user-friendly
		fmt.Fprintf(w, "  %s\n", badColor.Sprint(err.Error()))
		return
	}

	if len(manifest.Services) == 0 {
		fmt.Fprintln(w, "  (Manifest contains no services)")
		return
	}

	for _, service := range manifest.Services {
		fullComposePath := filepath.Join(configDir, service.ComposeFile)
		psCmd := exec.Command("docker", "compose", "-f", fullComposePath, "ps", "--format", "json")
		output, err := psCmd.Output()
		if err != nil {
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, warnColor.Sprint("⚠️  Could not get status"))
			continue
		}

		var containers []struct {
			State string `json:"State"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(output)))
		for decoder.More() {
			var c struct {
				State string `json:"State"`
			}
			if err := decoder.Decode(&c); err == nil {
				containers = append(containers, c)
			}
		}

		if len(containers) == 0 {
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, labelColor.Sprint("⚫ STOPPED"))
		} else {
			state := strings.ToUpper(containers[0].State)
			var statusStr string
			if strings.Contains(state, "RUNNING") || strings.Contains(state, "UP") {
				statusStr = goodColor.Sprintf("🟢 %s", state)
			} else if strings.Contains(state, "EXITED") || strings.Contains(state, "DEAD") {
				statusStr = badColor.Sprintf("🔴 %s", state)
			} else {
				statusStr = warnColor.Sprintf("🟡 %s", state)
			}
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, statusStr)
		}
	}
}

func colorizePercent(p float64) string {
	s := fmt.Sprintf("%.1f%%", p)
	if p > 90.0 {
		return badColor.Sprint(s)
	}
	if p > 75.0 {
		return warnColor.Sprint(s)
	}
	return goodColor.Sprint(s)
}

func colorizeTemp(t string) string {
	temp, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return t
	}
	s := fmt.Sprintf("%s°C", t)
	if temp > 85.0 {
		return badColor.Sprint(s)
	}
	if temp > 70.0 {
		return warnColor.Sprint(s)
	}
	return goodColor.Sprint(s)
}

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
	// Add the --no-color flag
	statusCmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colorized output")
}
