// cmd/call.go
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	callMethod  string
	callPort    int
	callTimeout int
	callData    string
)

var callCmd = &cobra.Command{
	Use:   "call <nodeIP> <path>",
	Short: "Make a direct HTTP call to a peer node via VPN",
	Long: `Sends an HTTP request directly to another Citadel node via the
Headscale VPN mesh. Used for inter-node communication, debugging,
and testing specific node endpoints.

The node must be reachable via its VPN IP address (100.64.x.x).

Examples:
  # Health check a peer
  citadel call 100.64.0.5 /health

  # Query a database service
  citadel call 100.64.0.5 /api/db/query --method POST --data '{"sql":"SELECT 1"}'

  # Check available services
  citadel call 100.64.0.5 /api/services`,
	Args: cobra.ExactArgs(2),
	Run:  runCall,
}

func runCall(cmd *cobra.Command, args []string) {
	nodeIP := args[0]
	path := args[1]

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	url := fmt.Sprintf("http://%s:%d%s", nodeIP, callPort, path)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(callTimeout)*time.Millisecond)
	defer cancel()

	var body io.Reader
	if callData != "" {
		body = strings.NewReader(callData)
	}

	req, err := http.NewRequestWithContext(ctx, callMethod, url, body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating request: %v\n", err)
		os.Exit(1)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fabric-Source", "citadel-cli")

	hostname, _ := os.Hostname()
	req.Header.Set("X-Fabric-Node-Id", hostname)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	duration := time.Since(start)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading response: %v\n", err)
		os.Exit(1)
	}

	// Print response metadata
	fmt.Printf("Status: %d %s\n", resp.StatusCode, resp.Status)
	fmt.Printf("Time:   %s\n", duration.Round(time.Millisecond))
	fmt.Println("---")

	// Pretty-print JSON if possible
	var prettyJSON map[string]interface{}
	if err := json.Unmarshal(respBody, &prettyJSON); err == nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(prettyJSON)
	} else {
		fmt.Println(string(respBody))
	}
}

func init() {
	rootCmd.AddCommand(callCmd)
	callCmd.Flags().StringVar(&callMethod, "method", "GET", "HTTP method (GET, POST, PUT, DELETE)")
	callCmd.Flags().IntVar(&callPort, "port", 8443, "Target port on the peer node")
	callCmd.Flags().IntVar(&callTimeout, "timeout", 30000, "Request timeout in milliseconds")
	callCmd.Flags().StringVar(&callData, "data", "", "Request body (JSON string)")
}
