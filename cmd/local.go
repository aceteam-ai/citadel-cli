// cmd/local.go
//
// `citadel local`: Citadel local mode (aceteam#13503). An agent harness on this
// computer drives a browser on this computer, with no AceTeam account. Every
// command in this group skips the root pre-run's network work (CA trust
// preparation, update check), so local mode makes no outbound calls by default.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
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

var localViewerPrint bool

var localViewerCmd = &cobra.Command{
	Use:   "viewer",
	Short: "Open the local co-browse viewer in your default browser",
	Long: `Asks the running Citadel local co-browse (started by your agent app) for a
fresh one-time viewer link and opens it in your default browser. The link
works once and expires after two minutes. Use --print to show it instead.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLocalViewer(cmd.Context(), localStateDir(), localViewerPrint, platform.OpenURL, cmd.OutOrStdout())
	},
}

func runLocalViewer(ctx context.Context, dir string, printOnly bool, open func(string) error, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	link, file, err := localviewer.RequestLaunch(ctx, dir, nil)
	if err != nil {
		return err
	}
	if printOnly {
		fmt.Fprintln(out, link)
		return nil
	}
	// Open the one-time launch file, never the link itself, so the code does
	// not appear on a command line other local users can read.
	if file == "" {
		fmt.Fprintln(out, "Open this link in your browser (it works once, for two minutes):")
		fmt.Fprintln(out, link)
		return nil
	}
	if err := open(file); err != nil {
		fmt.Fprintln(out, "Could not open a browser; open this link (it works once, for two minutes):")
		fmt.Fprintln(out, link)
		return nil
	}
	fmt.Fprintln(out, "Opened the Citadel viewer in your default browser.")
	return nil
}

func init() {
	rootCmd.AddCommand(localCmd)
	localCmd.AddCommand(localMCPCmd)
	localViewerCmd.Flags().BoolVar(&localViewerPrint, "print", false, "print the one-time link instead of opening it")
	localCmd.AddCommand(localViewerCmd)
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
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: localStateDir, viewer: true})
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
