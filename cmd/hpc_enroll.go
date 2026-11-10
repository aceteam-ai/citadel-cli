package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// hpcEnrollOptions are the inputs of one non-interactive enrollment
// (aceteam#12717). Every edge is a field so tests drive it offline.
type hpcEnrollOptions struct {
	TokenFile   string
	WallTimeEnd string
	BaseURL     string
	Client      hpc.Client
	Getenv      func(string) string
	Hostname    string
	GPUNames    func(visible []string) []string
	Now         time.Time
	UID         int
}

// hpcDeviceConfigPath is the job-local device config the worker reads.
func hpcDeviceConfigPath(nodeDir string) string {
	return filepath.Join(nodeDir, "config.yaml")
}

// readHPCCredentials loads the job-local enrollment, if any.
func readHPCCredentials(nodeDir string) (hpcCredentials, error) {
	return hpcCredentialsFrom(readDeviceConfigFromDirs([]string{nodeDir}))
}

// enrollHPCNode redeems the token and persists the response in the job root:
// the credential, org, API base and platform node id into the job-local
// config.yaml (read by the API-mode worker), and the fabric leaf next to the
// job-local node key. Labels are resolved before the token is read so a
// labeling problem never consumes the file.
func enrollHPCNode(ctx context.Context, layout hpc.Layout, o hpcEnrollOptions) (hpcCredentials, error) {
	if _, err := readHPCCredentials(layout.NodeDir); err == nil {
		return hpcCredentials{}, errors.New("this job root is already enrolled; a second enrollment would create a second node")
	}
	if strings.TrimSpace(o.TokenFile) == "" {
		return hpcCredentials{}, errors.New("--token-file is required")
	}
	labels, err := hpc.ResolveLabels(hpc.LabelInputs{
		Getenv:      o.Getenv,
		Hostname:    o.Hostname,
		GPUNames:    o.GPUNames,
		WallTimeEnd: o.WallTimeEnd,
		Now:         o.Now,
	})
	if err != nil {
		return hpcCredentials{}, err
	}
	token, err := hpc.ReadTokenFile(o.TokenFile, o.UID)
	if err != nil {
		return hpcCredentials{}, err
	}
	// The job-local key, never a key adopted from the shared home.
	store := nodeidentity.New(filepath.Join(layout.NodeDir, "identity"))
	key, err := store.GetOrCreateKey()
	if err != nil {
		return hpcCredentials{}, fmt.Errorf("job node key: %w", err)
	}
	csr, err := store.GenerateCSR(key)
	if err != nil {
		return hpcCredentials{}, err
	}
	resp, err := o.Client.Enroll(ctx, o.BaseURL, token, string(csr), labels)
	if err != nil {
		return hpcCredentials{}, err
	}
	if err := store.StoreLeaf(resp.LeafPEM, resp.ChainPEM); err != nil {
		return hpcCredentials{}, fmt.Errorf("store fabric leaf: %w", err)
	}
	if err := writeHPCDeviceConfig(layout.NodeDir, resp); err != nil {
		return hpcCredentials{}, err
	}
	return readHPCCredentials(layout.NodeDir)
}

// writeHPCDeviceConfig writes the enrollment into <nodeDir>/config.yaml with
// mode 0600 via a same-directory rename. The node dir is the job root's own,
// so no other process's keys are merged or preserved.
func writeHPCDeviceConfig(nodeDir string, r hpc.EnrollResponse) error {
	doc := map[string]string{
		"device_api_token": r.DeviceAPIToken,
		"api_base_url":     r.APIBaseURL,
		"org_id":           r.OrgID,
		"fabric_node_id":   r.NodeID,
		"hpc_node_uid":     r.NodeUID,
		"hpc_expires_at":   r.ExpiresAt,
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(nodeDir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("write job device config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), hpcDeviceConfigPath(nodeDir))
}

// nvidiaSMIGPUNames queries device names for the visible GPUs, best effort.
func nvidiaSMIGPUNames(visible []string) []string {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--query-gpu=index,uuid,name", "--format=csv,noheader").Output()
	if err != nil {
		return nil
	}
	return hpc.ParseGPUNames(string(out), visible)
}

// productionHPCEnrollOptions fills the live edges for a token file.
func productionHPCEnrollOptions(tokenFile, wallTimeEnd string) hpcEnrollOptions {
	host, _ := os.Hostname()
	return hpcEnrollOptions{
		TokenFile:   tokenFile,
		WallTimeEnd: wallTimeEnd,
		BaseURL:     authServiceURL,
		Client:      hpc.Client{HTTP: hpc.NewHTTPClient()},
		Getenv:      os.Getenv,
		Hostname:    host,
		GPUNames:    nvidiaSMIGPUNames,
		Now:         time.Now(),
		UID:         os.Geteuid(),
	}
}

var (
	hpcTokenFileFlag    string
	hpcWallTimeEndFlag  string
	hpcDrainTimeoutFlag time.Duration
)

var hpcCmd = &cobra.Command{
	Use:   "hpc",
	Short: "Run Citadel inside a batch (Slurm) allocation",
	Long: `Commands for a job-scoped, API-only node inside a batch allocation.
They always run under the hpc profile: state lives in a per-job directory,
and the node has no network listeners, mesh, shell, files or updates.`,
}

var hpcEnrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Enroll this allocation with a single-use token",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		layout := currentHPCLayout()
		creds, err := enrollHPCNode(cmd.Context(), layout, productionHPCEnrollOptions(hpcTokenFileFlag, hpcWallTimeEndFlag))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Enrolled node %s (job root %s)\n", creds.NodeID, layout.Root)
	},
}

var hpcDeregisterCmd = &cobra.Command{
	Use:   "deregister",
	Short: "Remove this allocation's node from the platform",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		creds, err := readHPCCredentials(currentHPCLayout().NodeDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(contextOrBackground(cmd.Context()), 15*time.Second)
		defer cancel()
		if err := (hpc.Client{HTTP: hpc.NewHTTPClient()}).Deregister(ctx, creds.APIBaseURL, creds.Token, creds.NodeID); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Deregistered node %s\n", creds.NodeID)
	},
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func init() {
	hpcEnrollCmd.Flags().StringVar(&hpcTokenFileFlag, "token-file", "", "Owner-only file holding the single-use enrollment token (removed after reading)")
	hpcEnrollCmd.Flags().StringVar(&hpcWallTimeEndFlag, "wall-time-end", "", "Allocation end (RFC 3339) when SLURM_JOB_END_TIME is not set")
	hpcCmd.AddCommand(hpcEnrollCmd, hpcDeregisterCmd)
	rootCmd.AddCommand(hpcCmd)

	workCmd.Flags().StringVar(&hpcTokenFileFlag, "hpc-token-file", "", "hpc profile: enroll with this token file at startup if the job is not enrolled")
	workCmd.Flags().StringVar(&hpcWallTimeEndFlag, "hpc-wall-time-end", "", "hpc profile: allocation end (RFC 3339) when SLURM_JOB_END_TIME is not set")
	workCmd.Flags().DurationVar(&hpcDrainTimeoutFlag, "hpc-drain-timeout", defaultHPCDrainTimeout, "hpc profile: how long to let in-flight jobs finish after the drain signal (SIGUSR1)")
}
