// cmd/status.go
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
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/fatih/color"
	"github.com/redis/go-redis/v9"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/spf13/cobra"
)

var (
	headerColor = color.New(color.FgCyan, color.Bold)
	goodColor   = color.New(color.FgGreen)
	warnColor   = color.New(color.FgYellow)
	badColor    = color.New(color.FgRed)
	labelColor  = color.New(color.Bold)
	noColor     bool // Flag to disable color
)

var statusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"st", "info"},
	Short:   "Shows a comprehensive status of the Citadel node",
	Long: `Provides a full health check and resource overview of the Citadel node.
It checks network connectivity, system vitals (CPU, RAM, Disk), GPU status,
and the state of all managed services.`,
	Example: `  # View full node status with colors
  citadel status

  # View status without colors (for scripts/logging)
  citadel status --no-color`,
	Run: func(cmd *cobra.Command, args []string) {
		// Handle the --no-color flag
		if noColor {
			color.NoColor = true
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		defer w.Flush()

		// Load manifest once for use in multiple sections
		manifest, _, _ := findAndReadManifest()

		headerColor.Fprintf(w, "--- 📊 Citadel Node Status (%s) ---\n", Version)

		headerColor.Fprintln(w, "\n💻 SYSTEM VITALS")
		printMemInfo(w)
		printCPUInfo(w)
		printDiskInfo(w)

		headerColor.Fprintln(w, "\n🗂️ CACHE USAGE (~/citadel-cache)")
		printCacheInfo(w)

		headerColor.Fprintln(w, "\n💎 GPU STATUS")
		printGPUInfo(w)

		headerColor.Fprintln(w, "\n🌐 NETWORK STATUS")
		printNetworkInfo(w, manifest)

		// Only show Redis section if configured
		if os.Getenv("REDIS_URL") != "" {
			headerColor.Fprintln(w, "\n📬 REDIS QUEUE")
			printRedisInfo(w)
		}

		headerColor.Fprintln(w, "\n🚀 MANAGED SERVICES")
		printServiceInfo(w)
	},
}

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

	totalCmd := exec.Command("du", "-sh", cacheDir)
	totalOutput, err := totalCmd.Output()
	if err == nil {
		parts := strings.Fields(string(totalOutput))
		if len(parts) > 0 {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Total Size"), parts[0])
		}
	}

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
	detector, err := platform.GetGPUDetector()
	if err != nil {
		fmt.Fprintf(w, "  GPU:\t%s\n", badColor.Sprintf("Error: %v", err))
		return
	}

	if !detector.HasGPU() {
		fmt.Fprintln(w, "  GPU:\tNo GPU detected on this system.")
		return
	}

	gpus, err := detector.GetGPUInfo()
	if err != nil {
		fmt.Fprintf(w, "  GPU:\t%s\n", warnColor.Sprintf("Hardware detected, but could not get details: %v", err))
		return
	}

	for i, gpu := range gpus {
		fmt.Fprintf(w, "  %s %d:\t%s\n", labelColor.Sprint("GPU"), i, gpu.Name)

		if gpu.Memory != "" {
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Memory"), gpu.Memory)
		}

		if gpu.Temperature != "" {
			// Parse temperature to colorize it
			tempStr := strings.TrimSuffix(gpu.Temperature, "°C")
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Temp"), colorizeTemp(tempStr))
		}

		if gpu.Utilization != "" {
			// Parse utilization to colorize it
			utilStr := strings.TrimSuffix(gpu.Utilization, "%")
			if utilFloat, err := strconv.ParseFloat(utilStr, 64); err == nil {
				fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Util"), colorizePercent(utilFloat))
			} else {
				fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Util"), gpu.Utilization)
			}
		}

		if gpu.Driver != "" {
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Driver"), gpu.Driver)
		}
	}
}

func printNetworkInfo(w *tabwriter.Writer, manifest *CitadelManifest) {
	// Check if we have network state
	if !network.HasState() {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Not logged in)"))
		fmt.Fprintf(w, "  %s\n", "   Run 'citadel login' to connect to the AceTeam Network")
		return
	}

	// Try to reconnect if state exists but not connected
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connected, reconnectErr := network.VerifyOrReconnect(ctx)
	if !connected {
		if reconnectErr != nil {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), warnColor.Sprintf("🟡 DISCONNECTED (%v)", reconnectErr))
		} else {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Could not reconnect)"))
		}
		fmt.Fprintf(w, "  %s\n", "   Run 'citadel login' to re-authenticate")
		return
	}

	// Get detailed status
	status, err := network.GetGlobalStatus(ctx)
	if err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), warnColor.Sprint("⚠️  WARNING (Could not get network status)"))
		return
	}

	if status.Connected {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), goodColor.Sprint("🟢 ONLINE to AceTeam Network"))
		if status.Hostname != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Hostname"), status.Hostname)
		}
		if status.IPv4 != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("IP Address"), status.IPv4)
		}
		// Display organization info from manifest
		if manifest != nil && manifest.Node.OrgID != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Organization"), manifest.Node.OrgID)
		}
		// Display node tags from manifest
		if manifest != nil && len(manifest.Node.Tags) > 0 {
			tagsStr := strings.Join(manifest.Node.Tags, ", ")
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Tags"), tagsStr)
		}
	} else {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Not connected to AceTeam Network)"))
	}
}

func printRedisInfo(w *tabwriter.Writer) {
	// Get Redis URL from environment (same as work command)
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		return
	}

	// Get queue name from environment or use default
	queueName := os.Getenv("WORKER_QUEUE")
	if queueName == "" {
		queueName = "jobs:v1:gpu-general"
	}

	consumerGroup := os.Getenv("CONSUMER_GROUP")
	if consumerGroup == "" {
		consumerGroup = "citadel-workers"
	}

	// Connect to Redis
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), warnColor.Sprintf("Invalid URL: %v", err))
		return
	}

	if password := os.Getenv("REDIS_PASSWORD"); password != "" {
		opts.Password = password
	}

	client := redis.NewClient(opts)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check connection
	if err := client.Ping(ctx).Err(); err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), badColor.Sprintf("❌ Connection failed: %v", err))
		return
	}

	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), goodColor.Sprint("🟢 Connected"))
	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Queue"), queueName)

	// Get queue length (XLEN)
	queueLen, err := client.XLen(ctx, queueName).Result()
	if err == nil {
		fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("Queue Length"), queueLen)
	}

	// Get pending count (XPENDING)
	pending, err := client.XPending(ctx, queueName, consumerGroup).Result()
	if err == nil && pending != nil {
		fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("Pending"), pending.Count)
	}

	// Get DLQ count
	dlqName := getDLQName(queueName)
	dlqLen, err := client.XLen(ctx, dlqName).Result()
	if err == nil {
		colorFn := goodColor
		if dlqLen > 0 {
			colorFn = warnColor
		}
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("DLQ Count"), colorFn.Sprintf("%d", dlqLen))
	}
}

func getDLQName(queueName string) string {
	parts := strings.Split(queueName, ":")
	suffix := parts[len(parts)-1]
	return fmt.Sprintf("dlq:v1:%s", suffix)
}

func printServiceInfo(w *tabwriter.Writer) {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// The error from findAndReadManifest is already user-friendly
		fmt.Fprintf(w, "  %s\n", badColor.Sprint(err.Error()))
		return
	}

	if len(manifest.Services) == 0 {
		fmt.Fprintln(w, "  No managed services are configured.")
		return
	}

	// If services are listed in the manifest, the 'services' directory must exist.
	servicesDir := filepath.Join(configDir, "services")
	if _, statErr := os.Stat(servicesDir); os.IsNotExist(statErr) {
		fmt.Fprintf(w, "  %s\n", warnColor.Sprint("⚠️  Configuration Error"))
		fmt.Fprintf(w, "    The configuration file lists services, but the 'services' directory is missing.\n")
		fmt.Fprintf(w, "    Expected at: %s\n", servicesDir)
		return
	}

	for _, service := range manifest.Services {
		fullComposePath := filepath.Join(configDir, service.ComposeFile)

		// Proactively check if the compose file exists to provide a better error message.
		if _, statErr := os.Stat(fullComposePath); os.IsNotExist(statErr) {
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, warnColor.Sprint("⚠️  Configuration Error"))
			fmt.Fprintf(w, "    Compose file not found: %s\n", service.ComposeFile)
			continue
		}

		psCmd := exec.Command("docker", "compose", "-f", fullComposePath, "ps", "--format", "json")
		output, err := psCmd.CombinedOutput() // Use CombinedOutput to get stderr
		if err != nil {
			errMsg := string(output)
			if strings.Contains(errMsg, "permission denied") && strings.Contains(errMsg, "docker.sock") {
				fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, badColor.Sprint("❌ PERMISSION DENIED"))
				fmt.Fprintf(w, "    %s\n", "Could not connect to the Docker daemon.")
				fmt.Fprintf(w, "    %s\n", "Hint: Add your user to the 'docker' group (`sudo usermod -aG docker $USER`)")
				fmt.Fprintf(w, "    %s\n", "      then log out and log back in for the change to take effect.")
			} else {
				fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, warnColor.Sprint("⚠️  Could not get status"))
				fmt.Fprintf(w, "    %s\n", strings.TrimSpace(errMsg))
			}
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
	statusCmd.Flags().BoolVar(&noColor, "no-color", false, "Disable colorized output")
}