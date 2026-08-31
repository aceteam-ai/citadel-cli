// cmd/service_expose.go
//
// `citadel service expose` — the node-local driver for gateway ingress (#598).
//
// The gateway runs INSIDE the `citadel work` process, so a separate CLI process
// cannot program it directly; it POSTs to that process's /agent/expose control
// endpoint instead. The request goes to the node's own mesh IP (not loopback)
// because the control surface authorizes VPN-origin callers — reusing the exact
// posture of the other /agent/* control endpoints rather than widening it.
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/spf13/cobra"
)

var (
	exposeSvcPort       int
	exposeSvcPath       string
	exposeSvcVisibility string
	exposeSvcTTL        time.Duration
)

var serviceExposeCmd = &cobra.Command{
	Use:   "expose <name>",
	Short: "Expose a local service or directory on the AceTeam Network gateway",
	Long: `Serves a local port, or a read-only static directory, on this node's gateway
at /expose/<name>/, gated by a visibility level. --port and --path are
mutually exclusive.

  private  only the creator (requires a caller identity the backend supplies,
           so a local CLI cannot grant it — use org or link)
  org      any member of your organization on the network
  link     anyone holding the signed, expiring link token

A --path directory is confined to this node's workspace
(--workspace/CITADEL_WORKSPACE on ` + "`citadel work`" + `) and served with an
auto-generated index when it has no index.html of its own — no need to run
your own web server.

Requires the node gateway to be running (citadel work).`,
	Example: `  # Expose the nvr module's Frigate UI to your organization
  citadel service expose frigate --port 8212 --visibility org

  # Share a dashboard by link for 2 hours
  citadel service expose grafana --port 3000 --visibility link --ttl 2h

  # Share a workspace directory (e.g. OCR results) by link
  citadel service expose scans --path results/ocr --visibility link --ttl 2h`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		vis := gateway.Visibility(exposeSvcVisibility)
		if !vis.Valid() {
			return fmt.Errorf("invalid --visibility %q (want private, org, or link)", exposeSvcVisibility)
		}
		hasPort := exposeSvcPort > 0
		hasPath := exposeSvcPath != ""
		if hasPort && hasPath {
			return fmt.Errorf("--port and --path are mutually exclusive; pick one source")
		}
		if !hasPort && !hasPath {
			return fmt.Errorf("--port or --path is required (what to expose)")
		}
		// A local caller cannot supply a remote tailnet identity, so `private`
		// would be inert (fails closed at the gateway). Say so rather than
		// creating an exposure nobody can reach.
		if vis == gateway.VisibilityPrivate {
			return fmt.Errorf("--visibility private needs a creator identity only the AceTeam backend can supply; use org or link from the CLI")
		}

		// meshIPv4() only answers inside the process that runs the network stack
		// (`citadel work`); from a separate CLI process it is empty, so fall back
		// to the facts that process persisted.
		facts := gatewayFactsForURL()
		ip := meshIPv4()
		if ip == "" {
			ip = facts.MeshIP
		}
		if ip == "" {
			return fmt.Errorf("cannot determine this node's AceTeam Network IP; is `citadel work` running?")
		}

		body, err := json.Marshal(status.ExposeSpec{
			Name:       name,
			Port:       exposeSvcPort,
			Path:       exposeSvcPath,
			Visibility: string(vis),
			TTLSeconds: int(exposeSvcTTL.Seconds()),
		})
		if err != nil {
			return err
		}

		url := fmt.Sprintf("http://%s:%d/agent/expose", ip, statusPortFrom(facts))
		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("reach the node agent at %s: %w (is `citadel work` running?)", url, err)
		}
		defer resp.Body.Close()

		var out struct {
			URL       string `json:"url"`
			Token     string `json:"token"`
			ExpiresAt string `json:"expires_at"`
			Error     string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return fmt.Errorf("decode agent response (HTTP %d): %w", resp.StatusCode, err)
		}
		if resp.StatusCode != http.StatusOK || out.Error != "" {
			msg := out.Error
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			return fmt.Errorf("expose failed: %s", msg)
		}

		if hasPath {
			fmt.Printf("\n✅ Exposed %q -> directory %s\n", name, exposeSvcPath)
		} else {
			fmt.Printf("\n✅ Exposed %q -> 127.0.0.1:%d\n", name, exposeSvcPort)
		}
		fmt.Printf("   URL:    %s\n", out.URL)
		fmt.Printf("   Access: %s\n", vis)
		if out.Token != "" {
			fmt.Printf("   Link:   %s?access_token=%s\n", out.URL, out.Token)
			fmt.Printf("   Expires: %s\n", out.ExpiresAt)
		}
		fmt.Println()
		return nil
	},
}

// statusPortFrom returns the port the agent control surface listens on, taking
// the running gateway's own recorded port when available.
func statusPortFrom(f gatewayFacts) int {
	if f.StatusPort > 0 {
		return f.StatusPort
	}
	if workStatusPort > 0 {
		return workStatusPort
	}
	return defaultWorkStatusPort
}

func init() {
	serviceExposeCmd.Flags().IntVar(&exposeSvcPort, "port", 0, "Local port the service listens on (mutually exclusive with --path)")
	serviceExposeCmd.Flags().StringVar(&exposeSvcPath, "path", "", "Workspace directory to share read-only, auto-indexed (mutually exclusive with --port)")
	serviceExposeCmd.Flags().StringVar(&exposeSvcVisibility, "visibility", "org", "Who may reach it: org or link")
	serviceExposeCmd.Flags().DurationVar(&exposeSvcTTL, "ttl", 24*time.Hour, "Lifetime of a --visibility link token")
	svcCmd.AddCommand(serviceExposeCmd)
}

// defaultWorkStatusPort mirrors the port `citadel work` binds the status/agent
// server on when the gateway is enabled (cmd/work.go).
const defaultWorkStatusPort = 8080
