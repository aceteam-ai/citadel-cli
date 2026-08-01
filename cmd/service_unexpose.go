// cmd/service_unexpose.go
//
// `citadel service unexpose` — revoke a gateway exposure (#647 follow-up).
//
// The inverse of `citadel service expose`. Same transport reasoning: the gateway
// lives inside the `citadel work` process, so this POSTs to that process's
// /agent/unexpose control endpoint over the node's own mesh IP rather than
// touching the gateway directly.
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

var serviceUnexposeCmd = &cobra.Command{
	Use:   "unexpose <name>",
	Short: "Revoke a service exposed on the AceTeam Network gateway",
	Long: `Stops serving /expose/<name>/ on this node's gateway and deletes its saved
record, so the service is unreachable now AND does not come back the next time
the node restarts.

Any outstanding 'link' tokens for the service stop working, because the gateway
fails closed for a name with no exposure policy.`,
	Example: `  # Stop sharing the nvr module's Frigate UI
  citadel service unexpose frigate`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// meshIPv4() only answers inside the process running the network stack;
		// from a separate CLI process fall back to the persisted gateway facts.
		facts := gatewayFactsForURL()
		ip := meshIPv4()
		if ip == "" {
			ip = facts.MeshIP
		}
		if ip == "" {
			return fmt.Errorf("cannot determine this node's AceTeam Network IP; is `citadel work` running?")
		}

		body, err := json.Marshal(map[string]string{"name": name})
		if err != nil {
			return err
		}

		url := fmt.Sprintf("http://%s:%d/agent/unexpose", ip, statusPortFrom(facts))
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
			Name       string `json:"name"`
			WasExposed bool   `json:"was_exposed"`
			Error      string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return fmt.Errorf("decode agent response (HTTP %d): %w", resp.StatusCode, err)
		}
		if resp.StatusCode != http.StatusOK || out.Error != "" {
			msg := out.Error
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			return fmt.Errorf("unexpose failed: %s", msg)
		}

		// Distinguish "revoked something" from "there was nothing to revoke".
		// Both are successes (revoke is idempotent), but claiming to have torn
		// down a live exposure that never existed would be a lie.
		if out.WasExposed {
			fmt.Printf("\n✅ Revoked %q — no longer served on the gateway.\n\n", name)
		} else {
			fmt.Printf("\n%q was not exposed; nothing to revoke.\n\n", name)
		}
		return nil
	},
}

func init() {
	svcCmd.AddCommand(serviceUnexposeCmd)
}
