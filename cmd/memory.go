package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/memory"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	memoryInstallForce bool
	memoryRecallScope  string
	memoryRecallQuery  string
	memoryCaptureNote  string
	memoryCaptureName  string
	memoryCaptureScope string
	memoryCaptureDesc  string
)

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Wire an AI client into AceTeam's shared memory",
	Long: `Connect this machine's AI coding client to AceTeam's shared memory so it
recalls durable context before each prompt and captures new facts after each turn.

'citadel memory install' authorizes this machine (device authorization) and
wires up Claude Code: it registers the AceTeam memory MCP server and adds hooks
that recall memory before a prompt and capture memory when a session ends.`,
}

// --- install ----------------------------------------------------------------

var memoryInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Authorize this machine and wire Claude Code into AceTeam memory",
	Long: `Runs the AceTeam device-authorization flow (no sudo required), stores a
scoped API key locally (user-only), and configures Claude Code to use AceTeam
memory via an MCP server entry plus recall/capture hooks.

Re-running is safe: an existing key is reused (use --force to re-authorize) and
Claude Code hooks/entries are never duplicated.`,
	Run: runMemoryInstall,
}

func runMemoryInstall(cmd *cobra.Command, args []string) {
	configDir := platform.ConfigDir()

	cfg, _ := memory.Load(configDir)

	if cfg == nil || cfg.APIKey == "" || memoryInstallForce {
		// Preflight: verify API reachability before an interactive prompt.
		if err := nexus.CheckAPIReachable(authServiceURL); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Cannot reach AceTeam: %v\n", err)
			os.Exit(1)
		}

		token, err := runMemoryDeviceAuthFlow(authServiceURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}

		cfg = &memory.Config{
			APIKey:     token.APIKey,
			APIBaseURL: authServiceURL,
			OrgID:      token.OrgID,
			OrgName:    token.OrgName,
			Scopes:     token.Scopes,
		}
		if err := memory.Save(configDir, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Could not save memory config: %v\n", err)
			os.Exit(1)
		}
		ok := color.New(color.FgGreen, color.Bold)
		ok.Println("✅ Authorized. Memory key saved.")
		if cfg.OrgName != "" {
			fmt.Printf("   Organization: %s\n", cfg.OrgName)
		}
		fmt.Printf("   Key file:     %s (user-only)\n", memory.ConfigPath(configDir))
	} else {
		fmt.Printf("Using existing memory key (%s). Re-run with --force to re-authorize.\n", memory.ConfigPath(configDir))
	}

	// Wire up Claude Code.
	home := userHomeDir()
	if !memory.DetectClaudeCode(home) {
		fmt.Println()
		color.New(color.FgYellow).Println("⚠ Claude Code not detected (~/.claude not found).")
		fmt.Println("  Your memory key is saved. Install Claude Code, then re-run:")
		fmt.Println("    citadel memory install")
		return
	}

	if err := wireClaudeCode(home, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ Claude Code wiring incomplete: %v\n", err)
		os.Exit(1)
	}
}

// wireClaudeCode registers the MCP server + recall/capture hooks in Claude
// Code's user config. Idempotent and additive.
func wireClaudeCode(home string, cfg *memory.Config) error {
	self := citadelBinaryPath()

	mcpChanged, err := memory.WriteMCPServer(
		memory.ClaudeJSONPath(home),
		memory.MCPServerName,
		cfg.EffectiveMCPURL(),
		cfg.APIKey,
	)
	if err != nil {
		return fmt.Errorf("write MCP server entry: %w", err)
	}

	settings := memory.ClaudeSettingsPath(home)
	recallCmd := fmt.Sprintf("%s memory recall", self)
	captureCmd := fmt.Sprintf("%s memory capture", self)

	recallChanged, err := memory.MergeHook(settings, "UserPromptSubmit", recallCmd, memory.RecallMarker, 10)
	if err != nil {
		return fmt.Errorf("merge recall hook: %w", err)
	}
	captureChanged, err := memory.MergeHook(settings, "SessionEnd", captureCmd, memory.CaptureMarker, 15)
	if err != nil {
		return fmt.Errorf("merge capture hook: %w", err)
	}

	fmt.Println()
	color.New(color.FgGreen, color.Bold).Println("✅ Claude Code wired into AceTeam memory")
	printWire("MCP server", memory.ClaudeJSONPath(home), memory.MCPServerName, mcpChanged)
	printWire("recall hook (UserPromptSubmit)", settings, memory.RecallMarker, recallChanged)
	printWire("capture hook (SessionEnd)", settings, memory.CaptureMarker, captureChanged)
	fmt.Println()
	fmt.Println("Restart Claude Code (or start a new session) to load the changes.")
	return nil
}

func printWire(label, path, detail string, changed bool) {
	state := "already present"
	mark := color.New(color.Faint).Sprint("=")
	if changed {
		state = "added"
		mark = color.New(color.FgGreen).Sprint("+")
	}
	fmt.Printf("   %s %-32s %s  (%s)\n", mark, label, state, filepath.Base(path))
}

// runMemoryDeviceAuthFlow runs the device-authorization flow with
// device_kind:"memory" and returns the minted act_ key on approval.
func runMemoryDeviceAuthFlow(authURL string) (*nexus.MemoryTokenResponse, error) {
	client := nexus.NewDeviceAuthClient(authURL)

	resp, err := client.StartFlow(&nexus.StartFlowOptions{DeviceKind: "memory"})
	if err != nil {
		return nil, fmt.Errorf("failed to start device authorization: %w", err)
	}

	// Non-TTY: plain text, poll without bubbletea.
	if !tui.IsTTY() {
		completeURL := resp.VerificationURI + "?code=" + resp.UserCode
		fmt.Println()
		fmt.Println("Device authorization required.")
		fmt.Printf("Open this URL to sign in: %s\n", completeURL)
		fmt.Printf("Or enter code manually:   %s\n", resp.UserCode)
		fmt.Println("\nWaiting for authorization...")
		token, err := client.PollForMemoryToken(resp.DeviceCode, resp.Interval)
		if err != nil {
			return nil, fmt.Errorf("device authorization failed: %w", err)
		}
		fmt.Println("Authorization successful!")
		return token, nil
	}

	// TTY: interactive bubbletea UI with background polling.
	model := ui.NewDeviceCodeModel(resp.UserCode, resp.VerificationURI, resp.ExpiresIn)
	program := ui.NewDeviceCodeProgram(model)

	tokenChan := make(chan *nexus.MemoryTokenResponse, 1)
	errChan := make(chan error, 1)

	go func() {
		token, err := client.PollForMemoryToken(resp.DeviceCode, resp.Interval)
		if err != nil {
			errChan <- err
			ui.UpdateStatus(program, "error:"+err.Error())
			return
		}
		tokenChan <- token
		ui.UpdateStatus(program, "approved")
	}()

	fmt.Println()
	if _, err := program.Run(); err != nil {
		return nil, fmt.Errorf("UI error: %w", err)
	}
	fmt.Println()

	select {
	case token := <-tokenChan:
		fmt.Println("✅ Authorization successful!")
		return token, nil
	case err := <-errChan:
		return nil, fmt.Errorf("device authorization failed: %w", err)
	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("device authorization was canceled")
	}
}

// --- recall ------------------------------------------------------------------

var memoryRecallCmd = &cobra.Command{
	Use:   "recall",
	Short: "Print recalled AceTeam memory for the current prompt (hook)",
	Long: `Queries AceTeam memory and prints a compact, token-bounded block to stdout.

Designed to be run by Claude Code's UserPromptSubmit hook: its stdout is
injected as context before your prompt. When run as a hook, the user's prompt
(read from the hook JSON on stdin) is used as the search query.

This command always exits 0 and prints nothing on error, so a memory outage or
revoked key never blocks your prompt.`,
	Run: runMemoryRecall,
}

func runMemoryRecall(cmd *cobra.Command, args []string) {
	// Fail-open: any failure prints nothing and exits 0.
	cfg, err := memory.Load(platform.ConfigDir())
	if err != nil || cfg == nil || cfg.APIKey == "" {
		os.Exit(0)
	}

	hook := readHookInput()

	query := memoryRecallQuery
	if query == "" && len(args) > 0 {
		query = strings.Join(args, " ")
	}
	if query == "" {
		query = hook.Prompt
	}
	// Default to searching ALL scopes (the memory_search "scope: null" contract);
	// a cwd-derived scope almost never matches real memory scopes (global,
	// project names). --scope narrows explicitly.
	scope := memoryRecallScope

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	block, err := memory.Recall(ctx, cfg, scope, query, memory.DefaultRecallBudget)
	if err != nil {
		// Silent by design (stderr only for debugging).
		fmt.Fprintf(os.Stderr, "citadel memory recall: %v\n", err)
		os.Exit(0)
	}
	if strings.TrimSpace(block) != "" {
		fmt.Println(block)
	}
	os.Exit(0)
}

// --- capture -----------------------------------------------------------------

var memoryCaptureCmd = &cobra.Command{
	Use:   "capture",
	Short: "Capture a durable note into AceTeam memory (hook)",
	Long: `Writes a durable note to AceTeam memory via memory_write.

Designed to be run by Claude Code's SessionEnd hook. For the demo it captures a
provided --note (or, if none, a short bounded tail of the session transcript
referenced by the hook JSON on stdin). Full turn-by-turn extraction and
deduplication is a follow-up (see PR description).

Always exits 0 so it never disrupts Claude Code.`,
	Run: runMemoryCapture,
}

func runMemoryCapture(cmd *cobra.Command, args []string) {
	cfg, err := memory.Load(platform.ConfigDir())
	if err != nil || cfg == nil || cfg.APIKey == "" {
		os.Exit(0)
	}

	hook := readHookInput()

	note := memoryCaptureNote
	if note == "" && len(args) > 0 {
		note = strings.Join(args, " ")
	}
	if note == "" {
		// Best-effort: a short bounded tail of the transcript, if available.
		note = transcriptTail(hook.TranscriptPath, 1500)
	}
	if strings.TrimSpace(note) == "" {
		// Nothing durable to record; exit quietly.
		os.Exit(0)
	}

	name := memoryCaptureName
	if name == "" {
		name = "claude-session-" + time.Now().Format("2006-01-02-1504")
	}
	// Empty scope lets memory_write default to "global" (predictable), rather
	// than an unstable cwd/worktree-basename scope. --scope narrows explicitly.
	scope := memoryCaptureScope
	desc := memoryCaptureDesc
	if desc == "" {
		desc = "Captured from a Claude Code session"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if _, err := memory.CaptureNote(ctx, cfg, name, note, desc, scope); err != nil {
		fmt.Fprintf(os.Stderr, "citadel memory capture: %v\n", err)
	}
	os.Exit(0)
}

// --- helpers -----------------------------------------------------------------

// hookInput is the subset of Claude Code's hook JSON (stdin) we consume.
type hookInput struct {
	Prompt         string `json:"prompt"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
}

// readHookInput reads and parses Claude Code hook JSON from stdin. It reads
// only when stdin is piped (not a TTY) so an interactive invocation never
// blocks. Any parse failure yields a zero-value struct.
func readHookInput() hookInput {
	var h hookInput
	if tui.IsTTY() {
		return h
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(data) == 0 {
		return h
	}
	_ = json.Unmarshal(data, &h)
	return h
}

// transcriptTail returns up to budget characters from the end of a Claude Code
// transcript file (best-effort; empty on any error).
func transcriptTail(path string, budget int) string {
	if path == "" || budget <= 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if len(s) > budget {
		s = s[len(s)-budget:]
	}
	return s
}

// citadelBinaryPath returns an absolute path to this binary for use in hook
// commands, falling back to "citadel" (resolved via PATH) if unavailable.
func citadelBinaryPath() string {
	if p, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return "citadel"
}

// userHomeDir resolves the invoking user's home directory, preferring the
// SUDO_USER's home when running under sudo so hooks (which run as the user) can
// read what install wrote.
func userHomeDir() string {
	if platform.IsRoot() {
		if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
			if home, err := platform.HomeDir(su); err == nil && home != "" {
				return home
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return os.Getenv("HOME")
}

func init() {
	rootCmd.AddCommand(memoryCmd)
	memoryCmd.AddCommand(memoryInstallCmd)
	memoryCmd.AddCommand(memoryRecallCmd)
	memoryCmd.AddCommand(memoryCaptureCmd)

	memoryInstallCmd.Flags().BoolVar(&memoryInstallForce, "force", false, "Re-authorize even if a memory key already exists")

	memoryRecallCmd.Flags().StringVar(&memoryRecallScope, "scope", "", "Restrict search to a memory scope (project name or 'global'; default: all scopes)")
	memoryRecallCmd.Flags().StringVar(&memoryRecallQuery, "query", "", "Search query (default: the prompt from the hook stdin)")

	memoryCaptureCmd.Flags().StringVar(&memoryCaptureNote, "note", "", "Note text to capture (default: a bounded transcript tail)")
	memoryCaptureCmd.Flags().StringVar(&memoryCaptureName, "name", "", "Memory slug (default: claude-session-<timestamp>)")
	memoryCaptureCmd.Flags().StringVar(&memoryCaptureScope, "scope", "", "Memory scope to write (project name or 'global'; default: global)")
	memoryCaptureCmd.Flags().StringVar(&memoryCaptureDesc, "description", "", "Short description for the memory frontmatter")
}
