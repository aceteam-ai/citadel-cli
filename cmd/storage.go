// cmd/storage.go
// On-node S3-compatible object storage (VersityGW), M1 of the object-storage
// epic (#466 / #469).
package cmd

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/storage"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Manage on-node S3-compatible object storage",
	Long: `Run an S3-compatible object store on this node, backed by VersityGW.

Objects are stored under ~/.citadel/storage/data and survive restarts. The
gateway is published on 127.0.0.1 only (never 0.0.0.0). Root credentials are
minted once on first start and reused on every restart, so stored objects and
presigned URLs remain valid.`,
}

var storageStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the object storage gateway",
	RunE:  runStorageStart,
}

var storageStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the object storage gateway status",
	RunE:  runStorageStatus,
}

var storageStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the object storage gateway",
	RunE:  runStorageStop,
}

func init() {
	rootCmd.AddCommand(storageCmd)
	storageCmd.AddCommand(storageStartCmd)
	storageCmd.AddCommand(storageStatusCmd)
	storageCmd.AddCommand(storageStopCmd)
}

func runStorageStart(cmd *cobra.Command, args []string) error {
	fmt.Println("Starting object storage gateway...")
	if err := storage.Start(); err != nil {
		return err
	}
	st, err := storage.GetStatus()
	if err != nil {
		return err
	}
	fmt.Printf("Storage gateway is running.\n")
	fmt.Printf("  Endpoint:   %s\n", st.Endpoint)
	fmt.Printf("  Access key: %s\n", st.AccessKey)
	fmt.Printf("  Data:       ~/.citadel/storage/data\n")
	fmt.Printf("\nRoot credentials are stored in ~/.citadel/storage/state.json.\n")
	fmt.Printf("Reach it with: aws --endpoint-url %s s3 ls\n", st.Endpoint)
	return nil
}

func runStorageStatus(cmd *cobra.Command, args []string) error {
	st, err := storage.GetStatus()
	if err != nil {
		return err
	}

	stateStr := color.New(color.FgRed).Sprint("stopped")
	if st.Running {
		if st.Healthy {
			stateStr = color.New(color.FgGreen).Sprint("running (healthy)")
		} else {
			stateStr = color.New(color.FgYellow).Sprint("running (unhealthy)")
		}
	}

	meshStr := st.MeshIP
	if meshStr == "" {
		meshStr = "unavailable (node not connected in this process)"
	}

	fmt.Printf("Object storage gateway\n")
	fmt.Printf("  Status:   %s\n", stateStr)
	fmt.Printf("  Endpoint: %s\n", st.Endpoint)
	fmt.Printf("  Mesh IP:  %s\n", meshStr)
	fmt.Printf("  Port:     %d\n", st.Port)
	fmt.Printf("  Buckets:  %d\n", st.BucketCount)
	fmt.Printf("  Used:     %s (approx)\n", humanBytes(st.BytesUsed))
	return nil
}

// humanBytes renders a byte count in a compact human-readable form (e.g.
// "1.5 MiB"). Kept local to avoid promoting the go-humanize indirect dependency.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func runStorageStop(cmd *cobra.Command, args []string) error {
	fmt.Println("Stopping object storage gateway...")
	if err := storage.Stop(); err != nil {
		return err
	}
	fmt.Println("Storage gateway stopped. Objects and credentials are preserved.")
	return nil
}
