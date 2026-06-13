// cmd/deploy.go
/*
Copyright © 2025 Jason Sun <jason@aceteam.ai>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	deployName    string
	deployPorts   []string
	deployEnv     []string
	deployGPU     bool
	deployRestart string
	deployNode    string
)

// validImageNameRe allows typical Docker image references:
// registry/repo:tag, repo:tag, repo@sha256:..., etc.
var validImageNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$`)

var deployCmd = &cobra.Command{
	Use:   "deploy [image]",
	Short: "Deploy a Docker container to a Citadel node",
	Long: `Pull a Docker image and run it as a managed container on this node or a remote node.

Local mode (default):
  1. Pulls the specified Docker image.
  2. Stops and removes any existing container with the same name.
  3. Starts a new container with the configured ports, env vars, and restart policy.

Remote mode (--node):
  Dispatches the deploy request to a remote node via the AceTeam API.`,
	Example: `  # Deploy nginx locally
  citadel deploy nginx:latest --port 8080:80

  # Deploy with GPU access and environment variables
  citadel deploy ghcr.io/my-org/my-model:v1 --gpu --env MODEL=llama3 --port 8000:8000

  # Deploy with a custom container name
  citadel deploy ollama/ollama --name my-ollama --port 11434:11434

  # Deploy to a remote node
  citadel deploy nginx:latest --node my-gpu-server --port 8080:80`,
	Args: cobra.ExactArgs(1),
	RunE: runDeploy,
}

func runDeploy(cmd *cobra.Command, args []string) error {
	image := args[0]

	// Validate image name
	if !validImageNameRe.MatchString(image) {
		return fmt.Errorf("invalid image name %q", image)
	}

	// Derive container name from image if not specified
	containerName := deployName
	if containerName == "" {
		containerName = deriveContainerName(image)
	}

	// Remote mode
	if deployNode != "" {
		return runRemoteDeploy(image, containerName)
	}

	// Local mode
	return runLocalDeploy(image, containerName)
}

// runLocalDeploy pulls an image and runs a container locally.
func runLocalDeploy(image, containerName string) error {
	stepColor := color.New(color.FgCyan)

	// Step 1: Pull the image
	stepColor.Printf("--- Pulling image: %s ---\n", image)
	pullCmd := exec.Command("docker", "pull", image)
	pullCmd.Stdout = os.Stdout
	pullCmd.Stderr = os.Stderr
	if err := pullCmd.Run(); err != nil {
		return fmt.Errorf("failed to pull image %q: %w", image, err)
	}
	fmt.Println()

	// Step 2: Stop and remove existing container (if any)
	inspectCmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerName)
	if output, err := inspectCmd.Output(); err == nil {
		status := strings.TrimSpace(string(output))
		stepColor.Printf("--- Stopping existing container '%s' (status: %s) ---\n", containerName, status)

		stopCmd := exec.Command("docker", "stop", containerName)
		stopCmd.Stdout = os.Stdout
		stopCmd.Stderr = os.Stderr
		_ = stopCmd.Run() // Ignore error if already stopped

		rmCmd := exec.Command("docker", "rm", containerName)
		rmCmd.Stdout = os.Stdout
		rmCmd.Stderr = os.Stderr
		_ = rmCmd.Run() // Ignore error if already removed
		fmt.Println()
	}

	// Step 3: Build docker run arguments
	runArgs := []string{"run", "-d", "--name", containerName, "--restart", deployRestart}

	// Port mappings
	for _, port := range deployPorts {
		runArgs = append(runArgs, "-p", port)
	}

	// Environment variables
	for _, env := range deployEnv {
		runArgs = append(runArgs, "-e", env)
	}

	// GPU access
	if deployGPU {
		runArgs = append(runArgs, "--runtime=nvidia", "--gpus", "all")
	}

	runArgs = append(runArgs, image)

	stepColor.Printf("--- Starting container '%s' ---\n", containerName)
	Debug("docker %s", strings.Join(runArgs, " "))

	runCmd := exec.Command("docker", runArgs...)
	output, err := runCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start container: %s", strings.TrimSpace(string(output)))
	}

	containerID := strings.TrimSpace(string(output))
	if len(containerID) > 12 {
		containerID = containerID[:12]
	}

	fmt.Println()
	color.New(color.FgGreen, color.Bold).Printf("Container '%s' is running (ID: %s)\n", containerName, containerID)

	// Print port mappings
	if len(deployPorts) > 0 {
		fmt.Println()
		fmt.Println("Port mappings:")
		for _, port := range deployPorts {
			fmt.Printf("  %s\n", port)
		}
	}

	fmt.Println()
	fmt.Println("Useful commands:")
	fmt.Printf("  citadel logs %s -f    - Follow container logs\n", containerName)
	fmt.Printf("  docker stop %s        - Stop the container\n", containerName)
	fmt.Printf("  docker rm %s          - Remove the container\n", containerName)

	return nil
}

// runRemoteDeploy dispatches a deploy request to a remote node via the AceTeam API.
func runRemoteDeploy(image, containerName string) error {
	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		return fmt.Errorf("remote deploy requires device authentication; run 'citadel init' or 'citadel login' first")
	}

	apiBaseURL := deviceConfig.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = authServiceURL
	}

	// Build the deploy payload
	payload := map[string]interface{}{
		"image":          image,
		"container_name": containerName,
		"restart_policy": deployRestart,
		"gpu":            deployGPU,
	}
	if len(deployPorts) > 0 {
		payload["ports"] = deployPorts
	}
	if len(deployEnv) > 0 {
		payload["env"] = deployEnv
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to build request payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/machines/%s/manage/deploy", apiBaseURL, deployNode)
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+deviceConfig.DeviceAPIToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fabric-Source", "citadel-cli")

	fmt.Printf("--- Deploying '%s' to node '%s' ---\n", image, deployNode)

	httpClient := &http.Client{Timeout: 120 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send deploy request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody := make([]byte, 1024)
		n, _ := resp.Body.Read(respBody)
		return fmt.Errorf("remote deploy failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody[:n])))
	}

	color.New(color.FgGreen, color.Bold).Printf("Deploy request sent to node '%s' successfully.\n", deployNode)
	fmt.Printf("  Image:     %s\n", image)
	fmt.Printf("  Container: %s\n", containerName)
	if len(deployPorts) > 0 {
		fmt.Printf("  Ports:     %s\n", strings.Join(deployPorts, ", "))
	}
	fmt.Println()
	fmt.Printf("Check status: citadel logs %s --node %s\n", containerName, deployNode)

	return nil
}

// deriveContainerName extracts a reasonable container name from a Docker image reference.
// Examples:
//
//	"nginx:latest"                  -> "citadel-nginx"
//	"ghcr.io/my-org/my-app:v1"     -> "citadel-my-app"
//	"ollama/ollama"                 -> "citadel-ollama"
func deriveContainerName(image string) string {
	// Strip tag or digest
	name := image
	if at := strings.Index(name, "@"); at != -1 {
		name = name[:at]
	}
	if colon := strings.LastIndex(name, ":"); colon != -1 {
		name = name[:colon]
	}

	// Take the last path component
	if slash := strings.LastIndex(name, "/"); slash != -1 {
		name = name[slash+1:]
	}

	// Sanitize: replace anything not alphanumeric/hyphen/dot with hyphens
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, name)

	if sanitized == "" {
		sanitized = "container"
	}

	return "citadel-" + sanitized
}

func init() {
	rootCmd.AddCommand(deployCmd)

	deployCmd.Flags().StringVar(&deployName, "name", "", "Container name (default: derived from image)")
	deployCmd.Flags().StringSliceVarP(&deployPorts, "port", "p", nil, "Port mappings (e.g. \"8080:80\")")
	deployCmd.Flags().StringSliceVarP(&deployEnv, "env", "e", nil, "Environment variables (e.g. \"KEY=value\")")
	deployCmd.Flags().BoolVar(&deployGPU, "gpu", false, "Enable GPU access (--runtime=nvidia --gpus all)")
	deployCmd.Flags().StringVar(&deployRestart, "restart", "unless-stopped", "Restart policy (no, always, unless-stopped, on-failure)")
	deployCmd.Flags().StringVar(&deployNode, "node", "", "Deploy to a remote node (node ID or hostname)")
}
