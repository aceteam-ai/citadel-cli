// cmd/local.go
//
// `citadel local`: Citadel local mode (aceteam#13503). An agent harness on this
// computer drives a browser on this computer, with no AceTeam account. Every
// command in this group skips the root pre-run's network work (CA trust
// preparation, update check), so local mode makes no outbound calls by default.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

var localCmd = &cobra.Command{
	Use:   "local",
	Short: "Local mode: let AI agents on this computer drive a browser here, no account needed",
	Long: `Citadel local mode lets an AI agent app on this computer (any MCP-capable
harness) drive a browser on this computer. You watch in a local viewer, approve
the agent's actions, and take over to log in yourself; the agent never sees
what you type. No AceTeam account, Docker or admin rights are needed, and local
mode makes no network calls of its own.`,
}

var localMCPCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve the local tools over MCP (stdio) for an agent on this computer",
	Long: `Starts a Model Context Protocol server on stdin/stdout that serves only
Citadel's local tools, including local_cobrowse_*. It never reads an AceTeam API
key or device token and never contacts AceTeam.

Point your harness at it with the absolute path to citadel, for example:
  {"mcpServers": {"citadel": {"command": "C:\\Users\\YOU\\AppData\\Local\\Citadel\\citadel.exe", "args": ["local", "mcp"]}}}`,
	Args: cobra.NoArgs,
	RunE: runLocalMCP,
}

func init() {
	rootCmd.AddCommand(localCmd)
	localCmd.AddCommand(localMCPCmd)
}

// errLocalModeNoBackend is what the local-only bridge's HTTP client returns if
// anything ever tries to use it. Nothing should; the bridge never forwards.
var errLocalModeNoBackend = errors.New("citadel local mode makes no backend requests")

type refuseTransport struct{}

func (refuseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errLocalModeNoBackend
}

// newLocalOnlyBridge builds the `citadel local mcp` bridge. transport is the
// only HTTP path the bridge holds; tests pass a recording one to prove it is
// never used.
func newLocalOnlyBridge(tools []localMCPTool, rt *localCobrowseRuntime, in io.Reader, out io.Writer, transport http.RoundTripper) *mcpBridge {
	if transport == nil {
		transport = refuseTransport{}
	}
	b := &mcpBridge{
		localOnly:  true,
		localTools: tools,
		stdin:      in,
		stdout:     out,
		httpClient: &http.Client{Transport: transport},
	}
	if rt != nil {
		b.onClientName = rt.setAgent
	}
	return b
}

func runLocalMCP(cmd *cobra.Command, args []string) error {
	debugToStderr = true
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: network.GetNodeConfigDir})
	defer rt.close()

	// A harness usually stops its MCP server by closing stdin, which ends run.
	// On a signal, stop the browser sessions before exiting so no logged-in
	// browser outlives the server (Windows also kills it through the Job Object).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		rt.close()
		os.Exit(0)
	}()

	deps := realLocalMCPDeps()
	deps.cobrowse = rt
	fmt.Fprintln(os.Stderr, "Citadel local mode: serving local tools only (no AceTeam account, no network calls of its own).")
	return newLocalOnlyBridge(newLocalMCPTools(deps), rt, os.Stdin, os.Stdout, nil).run()
}
