// cmd/mesh.go
//
// `citadel mesh` — discover and chat with models served by OTHER citadel nodes
// over the mesh (issue #576, Phase 2 of citadel-chat-and-mesh).
//
// This is the thin CLI surface over the standalone internal/mesh discovery +
// routing layer. It is deliberately NOT `citadel chat` so it composes with, and
// never collides with, Phase 1's local chat REPL (#575) — that command can later
// import internal/mesh to add a remote/peer selection mode using the same
// Inventory/Client this command drives.
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/mesh"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

var (
	meshPort       int
	meshModelsJSON bool
	meshChatNode   string
	meshChatModel  string
	meshTimeout    int
)

// meshDiscoverTimeout bounds peer discovery. Individual probes are already
// bounded (~4s) with capped concurrency, so this is only an outer safety net;
// it is kept separate from the chat generation budget so a slow completion is
// never cut short by discovery time.
const meshDiscoverTimeout = 30 * time.Second

var meshCmd = &cobra.Command{
	Use:   "mesh",
	Short: "Discover and chat with models on other nodes over the mesh",
	Long: `Discover the models served by OTHER citadel nodes on the AceTeam Network
(the mesh) and route chat requests to them directly, node-to-node.

Each node publishes its served models at its /status endpoint over the mesh
(served by ` + "`citadel work --gateway`" + `). ` + "`citadel mesh`" + ` enumerates online peers,
probes each one's /status, and aggregates a fabric-wide model -> node/engine/port
view. Peers that are unreachable (not serving the status endpoint on the mesh)
are skipped gracefully.`,
}

var meshModelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List models served across reachable mesh nodes",
	Long: `Enumerates online peers, probes each node's /status over the mesh, and lists
every served model as a model -> node/engine/port row.

Only OTHER nodes are shown (this node is excluded). Nodes that do not serve the
status endpoint on the mesh (e.g. plain ` + "`citadel work`" + ` without --gateway) are
skipped.`,
	Example: `  # List all models served across the mesh
  citadel mesh models

  # JSON output (includes unreachable nodes and their errors)
  citadel mesh models --json

  # Probe a non-default status port
  citadel mesh models --port 8080`,
	Run: runMeshModels,
}

var meshChatCmd = &cobra.Command{
	Use:   "chat [prompt]",
	Short: "Send a one-shot chat request to a model on another node",
	Long: `Discovers models across the mesh, selects one by --node and/or --model, and
sends a single OpenAI chat-completion request to that node's engine over the
mesh, printing the assistant's reply.

This is a minimal, non-interactive surface (the interactive REPL is Phase 1,
#575). Selection rules:
  - If --model uniquely identifies one served model, --node is optional.
  - If a model is served on multiple nodes, use --node (hostname or IP) to pick.
  - If a node serves exactly one model, --model is optional.

REACHABILITY: discovery works today, but routing the chat request requires the
target node to expose a chat endpoint on the mesh. On embedded-tsnet nodes the
engine's host port is NOT reachable over the mesh (only ports citadel binds a VPN
listener for answer), and no node gateway yet proxies /v1/chat/completions to the
local engine, so this command currently fails with connection-refused against
such nodes. Adding the node-side gateway chat route is tracked in #581.`,
	Example: `  # Chat with a uniquely-named model anywhere on the mesh
  citadel mesh chat --model Qwen/Qwen2.5-7B "Explain WireGuard in one line"

  # Pick a specific node when a model is served in several places
  citadel mesh chat --node gpu-node-1 --model llama3 "hello"

  # Node serves a single model
  citadel mesh chat --node gpu-node-1 "hello"`,
	Args: cobra.MinimumNArgs(1),
	Run:  runMeshChat,
}

// meshDiscover connects to the mesh and runs discovery, returning the aggregated
// inventory. Shared by the models and chat subcommands.
func meshDiscover(ctx context.Context) (*mesh.Inventory, error) {
	if err := ensureNetworkConnected(ctx); err != nil {
		return nil, err
	}

	lister := func(ctx context.Context) ([]mesh.Peer, error) {
		peers, err := network.GetGlobalPeers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]mesh.Peer, 0, len(peers))
		for _, p := range peers {
			out = append(out, mesh.Peer{Hostname: p.Hostname, IP: p.IP, Online: p.Online})
		}
		return out, nil
	}

	selfIP, _ := network.GetGlobalIPv4()

	return mesh.Discover(ctx, lister, network.Dial, mesh.Options{
		Port:   meshPort,
		SelfIP: selfIP,
	})
}

func runMeshModels(cmd *cobra.Command, args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), meshDiscoverTimeout)
	defer cancel()

	inv, err := meshDiscover(ctx)
	if err != nil {
		badColor.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if meshModelsJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(inv); err != nil {
			badColor.Fprintf(os.Stderr, "Failed to encode JSON: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(inv.Models) == 0 {
		fmt.Println("No models found on reachable mesh nodes.")
		reachable := 0
		for _, n := range inv.Nodes {
			if n.Reachable {
				reachable++
			}
		}
		fmt.Printf("(%d peer(s) probed, %d reachable)\n", len(inv.Nodes), reachable)
		fmt.Println("Nodes must serve the status endpoint on the mesh (run 'citadel work --gateway').")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "MODEL\tNODE\tENGINE\tADDRESS")
	fmt.Fprintln(w, "-----\t----\t------\t-------")
	for _, m := range inv.Models {
		engine := m.Engine
		if engine == "" {
			engine = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s:%d\n", m.Model, m.Hostname, engine, m.IP, m.Port)
	}
	w.Flush()

	fmt.Printf("\n%d model(s) across %d reachable node(s).\n", len(inv.Models), countReachable(inv))
}

func countReachable(inv *mesh.Inventory) int {
	n := 0
	for _, node := range inv.Nodes {
		if node.Reachable {
			n++
		}
	}
	return n
}

func runMeshChat(cmd *cobra.Command, args []string) {
	prompt := args[0]
	for _, a := range args[1:] {
		prompt += " " + a
	}

	// Discovery and generation get SEPARATE budgets: a slow completion must not
	// be cut short because peer discovery ate into a single shared deadline.
	discCtx, discCancel := context.WithTimeout(context.Background(), meshDiscoverTimeout)
	inv, err := meshDiscover(discCtx)
	discCancel()
	if err != nil {
		badColor.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	target, err := inv.FindModel(meshChatNode, meshChatModel)
	if err != nil {
		badColor.Fprintf(os.Stderr, "Error: %v\n", err)
		if len(inv.Models) > 0 {
			fmt.Fprintln(os.Stderr, "\nAvailable models:")
			for _, m := range inv.Models {
				fmt.Fprintf(os.Stderr, "  - %s on %s (%s:%d)\n", m.Model, m.Hostname, m.IP, m.Port)
			}
		}
		os.Exit(1)
	}

	warnColor.Fprintf(os.Stderr, "→ %s on %s (%s:%d)\n", target.Model, target.Hostname, target.IP, target.Port)

	body, err := mesh.BuildChatRequest(target.Model, prompt, false)
	if err != nil {
		badColor.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	chatCtx, chatCancel := context.WithTimeout(context.Background(), time.Duration(meshTimeout)*time.Second)
	defer chatCancel()

	client := mesh.NewClient(network.Dial)
	resp, err := client.ChatCompletionTo(chatCtx, target, body)
	if err != nil {
		badColor.Fprintf(os.Stderr, "Request failed: %v\n", err)
		// A refused connection to a discovered engine port is the EXPECTED state
		// today: embedded-tsnet does not forward inbound mesh traffic to a node's
		// host-bound engine port (only ports citadel ListenVPNs answer over the
		// mesh), and no node gateway yet proxies /v1/chat/completions to the local
		// engine. Discovery still works; the reachable chat endpoint is the
		// follow-up. Make that explicit rather than leaving a bare dial error.
		if isConnRefused(err) {
			warnColor.Fprintln(os.Stderr,
				"\nThe target node has no mesh-reachable chat endpoint yet. Its engine port is\n"+
					"bound to the host but embedded-tsnet does not expose it on the mesh, and the\n"+
					"node gateway does not yet proxy /v1/chat/completions to the local engine.\n"+
					"Tracking: https://github.com/aceteam-ai/citadel-cli/issues/581")
		}
		os.Exit(1)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		badColor.Fprintf(os.Stderr, "Engine returned %d: %s\n", resp.StatusCode, string(respBody))
		os.Exit(1)
	}

	reply, err := extractAssistantReply(respBody)
	if err != nil {
		// Fall back to raw JSON so the user still sees the engine's output.
		fmt.Println(string(respBody))
		return
	}
	fmt.Println(reply)
}

// isConnRefused reports whether err is a connection-refused dial failure, the
// expected outcome when a discovered engine port is not exposed on the mesh.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "connection refused")
}

// extractAssistantReply pulls the assistant message content out of an OpenAI
// chat-completions response.
func extractAssistantReply(body []byte) (string, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return parsed.Choices[0].Message.Content, nil
}

func init() {
	rootCmd.AddCommand(meshCmd)
	meshCmd.AddCommand(meshModelsCmd)
	meshCmd.AddCommand(meshChatCmd)

	meshCmd.PersistentFlags().IntVar(&meshPort, "port", mesh.DefaultStatusPort, "Peer status endpoint port to probe over the mesh")

	meshModelsCmd.Flags().BoolVar(&meshModelsJSON, "json", false, "Output in JSON format (includes unreachable nodes)")

	meshChatCmd.Flags().StringVar(&meshChatNode, "node", "", "Target node hostname or mesh IP")
	meshChatCmd.Flags().StringVar(&meshChatModel, "model", "", "Model to chat with")
	meshChatCmd.Flags().IntVar(&meshTimeout, "timeout", 120, "Chat generation timeout in seconds")
}
