// cmd/logs.go
/*
Copyright © 2025 Jason Sun <jason@aceteam.ai>
*/
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/spf13/cobra"
)

var (
	logsFollow bool
	logsTail   string
	logsLines  int
	logsSince  string
	logsNode   string
)

// validServiceNameRe restricts service names to safe characters (alphanumeric, hyphens, dots, underscores).
// This prevents argument injection when passing the name to journalctl or docker commands.
var validServiceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// logsCmd represents the logs command
var logsCmd = &cobra.Command{
	Use:   "logs [service]",
	Short: "View service logs from a Citadel node",
	Long: fmt.Sprintf(`View logs from Docker services, systemd units, or remote nodes.

Resolution order:
  1. If --node is set, fetch logs from a remote node via the AceTeam API.
  2. If the service is found in the citadel.yaml manifest, use docker compose logs.
  3. If a citadel-<service> Docker container exists, use docker logs.
  4. Otherwise, fall back to journalctl -u <service>.

If no service is specified, defaults to "citadel" (the systemd unit).

Available managed services: %s`, strings.Join(services.GetAvailableServices(), ", ")),
	Example: `  # View the citadel systemd service logs
  citadel logs

  # View last 100 lines of vllm logs (Docker)
  citadel logs vllm

  # Follow ollama logs in real-time
  citadel logs ollama -f

  # View last 50 lines and follow
  citadel logs llamacpp -f -t 50

  # View systemd logs since 1 hour ago
  citadel logs citadel --since 1h -n 200

  # Fetch logs from a remote node
  citadel logs citadel --node my-gpu-server`,
	Args: cobra.MaximumNArgs(1),
	RunE: runLogs,
}

func runLogs(cmd *cobra.Command, args []string) error {
	serviceName := "citadel"
	if len(args) > 0 {
		serviceName = args[0]
	}

	// Validate the service name to prevent argument injection
	if !validServiceNameRe.MatchString(serviceName) {
		return fmt.Errorf("invalid service name %q: must be alphanumeric with hyphens, dots, or underscores", serviceName)
	}

	// Resolve the effective line count: --lines/-n takes precedence over --tail/-t
	effectiveTail := logsTail
	if cmd.Flags().Changed("lines") {
		effectiveTail = fmt.Sprintf("%d", logsLines)
	}

	// Remote mode: fetch logs from a remote node via AceTeam API
	if logsNode != "" {
		return runRemoteLogs(serviceName, effectiveTail)
	}

	// Local mode: try manifest -> container -> journalctl

	// 1. Try to find the service in the manifest (docker compose)
	var fullComposePath string
	manifest, configDir, err := findAndReadManifest()
	if err == nil {
		for _, s := range manifest.Services {
			if s.Name == serviceName {
				fullComposePath = filepath.Join(configDir, s.ComposeFile)
				break
			}
		}
	}

	if fullComposePath != "" {
		return runDockerComposeLogs(serviceName, fullComposePath, effectiveTail)
	}

	// 2. Try direct container access (for 'citadel run' services)
	containerName := fmt.Sprintf("citadel-%s", serviceName)
	inspectCmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerName)
	if _, err := inspectCmd.Output(); err == nil {
		return runDockerLogs(serviceName, containerName, effectiveTail)
	}

	// 3. Fall back to journalctl (systemd)
	return runJournalctlLogs(serviceName, effectiveTail)
}

// runDockerComposeLogs streams logs from a docker compose service.
func runDockerComposeLogs(serviceName, composePath, tailLines string) error {
	dockerArgs := []string{"compose", "-f", composePath, "logs"}
	if logsFollow {
		dockerArgs = append(dockerArgs, "--follow")
	}
	if tailLines != "" {
		dockerArgs = append(dockerArgs, "--tail", tailLines)
	}
	if logsSince != "" {
		dockerArgs = append(dockerArgs, "--since", logsSince)
	}

	logCmd := exec.Command("docker", dockerArgs...)
	// Inject CITADEL_WORKSPACE + host-port vars so compose files guarded with
	// ${VAR:?...} (transcribe/meeting workspace mount, #525) interpolate.
	logCmd.Env = composeEnv()
	logCmd.Stdout = os.Stdout
	logCmd.Stderr = os.Stderr

	fmt.Printf("--- Streaming logs for service '%s' (Ctrl+C to stop) ---\n", serviceName)
	return handleLogCmdError(logCmd.Run())
}

// runDockerLogs streams logs from a standalone Docker container.
func runDockerLogs(serviceName, containerName, tailLines string) error {
	dockerArgs := []string{"logs"}
	if logsFollow {
		dockerArgs = append(dockerArgs, "-f")
	}
	if tailLines != "" {
		dockerArgs = append(dockerArgs, "--tail", tailLines)
	}
	if logsSince != "" {
		dockerArgs = append(dockerArgs, "--since", logsSince)
	}
	dockerArgs = append(dockerArgs, containerName)

	logCmd := exec.Command("docker", dockerArgs...)
	logCmd.Stdout = os.Stdout
	logCmd.Stderr = os.Stderr

	fmt.Printf("--- Streaming logs for service '%s' (Ctrl+C to stop) ---\n", serviceName)
	return handleLogCmdError(logCmd.Run())
}

// runJournalctlLogs streams logs from a systemd unit via journalctl.
func runJournalctlLogs(serviceName, tailLines string) error {
	journalArgs := []string{"-u", serviceName, "--no-pager"}

	if tailLines != "" {
		journalArgs = append(journalArgs, "-n", tailLines)
	} else {
		journalArgs = append(journalArgs, "-n", "100")
	}

	if logsSince != "" {
		journalArgs = append(journalArgs, "--since", parseRelativeDuration(logsSince))
	}

	if logsFollow {
		journalArgs = append(journalArgs, "-f")
	}

	logCmd := exec.Command("journalctl", journalArgs...)
	logCmd.Stdout = os.Stdout
	logCmd.Stderr = os.Stderr

	fmt.Printf("--- Streaming logs for service '%s' via journalctl (Ctrl+C to stop) ---\n", serviceName)
	return handleLogCmdError(logCmd.Run())
}

// runRemoteLogs fetches logs from a remote node via the AceTeam API.
func runRemoteLogs(serviceName, tailLines string) error {
	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		return fmt.Errorf("remote logs require device authentication; run 'citadel init' or 'citadel login' first")
	}

	apiBaseURL := deviceConfig.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = authServiceURL
	}

	// Build the request URL for the remote logs endpoint
	endpoint := fmt.Sprintf("%s/api/machines/%s/manage/logs", apiBaseURL, logsNode)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	q := req.URL.Query()
	q.Set("service", serviceName)
	if tailLines != "" {
		q.Set("lines", tailLines)
	}
	if logsSince != "" {
		q.Set("since", logsSince)
	}
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Authorization", "Bearer "+deviceConfig.DeviceAPIToken)
	req.Header.Set("X-Fabric-Source", "citadel-cli")

	fmt.Printf("--- Fetching logs for service '%s' from node '%s' ---\n", serviceName, logsNode)

	httpClient := &http.Client{Timeout: 30 * time.Second}

	if logsFollow {
		return pollRemoteLogs(httpClient, req, serviceName)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch remote logs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remote logs request failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Try to parse as JSON (API may return structured response)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var logResp struct {
		Logs string `json:"logs"`
	}
	if json.Unmarshal(body, &logResp) == nil && logResp.Logs != "" {
		fmt.Print(logResp.Logs)
	} else {
		fmt.Print(string(body))
	}

	return nil
}

// pollRemoteLogs polls the remote logs endpoint every 2 seconds for new lines.
func pollRemoteLogs(httpClient *http.Client, baseReq *http.Request, serviceName string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var lastLineCount int
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	fmt.Printf("--- Following logs for service '%s' from node '%s' (Ctrl+C to stop) ---\n", serviceName, logsNode)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			req := baseReq.Clone(ctx)
			resp, err := httpClient.Do(req)
			if err != nil {
				Debug("remote log poll error: %v", err)
				continue
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				Debug("remote log poll error: status=%d", resp.StatusCode)
				continue
			}

			lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
			if len(lines) > lastLineCount {
				// Print only new lines
				for _, line := range lines[lastLineCount:] {
					fmt.Println(line)
				}
				lastLineCount = len(lines)
			}
		}
	}
}

// parseRelativeDuration converts a simple relative duration string (e.g. "5m", "1h", "24h")
// into a journalctl-compatible --since value (e.g. "5 minutes ago").
func parseRelativeDuration(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil {
		// Not a Go duration; pass through as-is (let journalctl validate)
		return s
	}

	since := time.Now().Add(-d)
	return since.Format("2006-01-02 15:04:05")
}

// handleLogCmdError handles errors from log streaming commands,
// suppressing expected exit codes (e.g. SIGINT / Ctrl+C).
func handleLogCmdError(err error) error {
	if err == nil {
		return nil
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		// Exit code 130 = SIGINT (Ctrl+C) — expected when following logs
		if exitError.ExitCode() == 130 {
			return nil
		}
	}
	return err
}

func init() {
	rootCmd.AddCommand(logsCmd)

	// Flags for controlling log output
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")
	logsCmd.Flags().StringVarP(&logsTail, "tail", "t", "100", "Number of lines to show from the end of the logs")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 100, "Number of log lines to show")
	logsCmd.Flags().StringVar(&logsSince, "since", "", "Show logs since duration (e.g. \"5m\", \"1h\", \"24h\")")
	logsCmd.Flags().StringVar(&logsNode, "node", "", "Fetch logs from a remote node (node ID or hostname)")
}
