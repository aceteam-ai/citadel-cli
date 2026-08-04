package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	memoryInstallForce       bool
	memoryUninstallConfirmed bool
	memoryRecallScope        string
	memoryRecallQuery        string
	memoryCaptureNote        string
	memoryCaptureName        string
	memoryCaptureScope       string
	memoryCaptureDesc        string
)

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Wire an AI client into AceTeam's shared memory",
	Long: `Connect this machine's AI coding client to AceTeam's shared memory so it
recalls durable context before each prompt and captures a bounded session summary
when the session ends.

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

Re-running is safe: an existing key is reused and Claude Code hooks/entries are
never duplicated. Rotation fails closed until backend revocation is available.`,
	Run: runMemoryInstall,
}

func runMemoryInstall(cmd *cobra.Command, args []string) {
	if err := memoryElevatedUserCheck(platform.IsRoot(), os.Getenv("SUDO_USER")); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	configDir := platform.ConfigDir()

	cfg, err := memory.Load(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Refusing to replace an unreadable memory credential: %v\n", err)
		os.Exit(1)
	}

	needsAuthorization, lifecycleErr := memoryAuthorizationNeeded(cfg, memoryInstallForce)
	if lifecycleErr != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", lifecycleErr)
		os.Exit(1)
	}
	if needsAuthorization {
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
		if err := memory.ValidateScopes(token.Scopes); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Refusing unsafe memory credential: %v\n", err)
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
		if err := cfg.ValidateCredential(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Refusing unsafe existing memory credential: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Using existing memory key (%s).\n", memory.ConfigPath(configDir))
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

// memoryAuthorizationNeeded fails closed for rotation. The current memory
// device-auth response does not return a key id and the scoped key has no
// self-revocation authority, so minting a replacement would orphan a live
// bearer. Rotation stays disabled until the backend offers an atomic contract.
func memoryAuthorizationNeeded(cfg *memory.Config, force bool) (bool, error) {
	hasKey := cfg != nil && strings.TrimSpace(cfg.APIKey) != ""
	if force && hasKey {
		return false, errors.New("memory key rotation is unavailable: the backend cannot revoke the existing scoped key; revoke it in AceTeam API Keys, run 'citadel memory uninstall --confirmed-revoked', then install again")
	}
	return !hasKey, nil
}

// wireClaudeCode registers the MCP server + recall/capture hooks in Claude
// Code's user config. Idempotent and additive.
func wireClaudeCode(home string, cfg *memory.Config) error {
	self := citadelBinaryPath()
	quotedSelf, err := quoteHookArg(self)
	if err != nil {
		return fmt.Errorf("quote hook executable: %w", err)
	}

	mcpChanged, err := memory.WriteMCPServer(
		memory.ClaudeJSONPath(home),
		memory.MCPServerName,
		self,
		[]string{"--no-auto-update", "mcp", "--memory-config"},
	)
	if err != nil {
		return fmt.Errorf("write MCP server entry: %w", err)
	}

	settings := memory.ClaudeSettingsPath(home)
	recallCmd := fmt.Sprintf("%s memory recall --no-auto-update", quotedSelf)
	captureCmd := fmt.Sprintf("%s memory capture --no-auto-update", quotedSelf)

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

// --- uninstall --------------------------------------------------------------

var memoryUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the Claude Code memory integration after remote revocation",
	Long: `Removes the Citadel memory MCP entry, recall/capture hooks, and local
memory credential. This command fails closed while a local key exists because
the current backend does not provide scoped-key self-revocation.

First revoke the corresponding "Memory" key in AceTeam API Keys. Then rerun
with --confirmed-revoked to acknowledge that the remote bearer is inactive.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := memoryElevatedUserCheck(platform.IsRoot(), os.Getenv("SUDO_USER")); err != nil {
			return err
		}
		if err := uninstallMemory(userHomeDir(), platform.ConfigDir(), memoryUninstallConfirmed); err != nil {
			return err
		}
		fmt.Println("✅ Claude Code memory integration removed")
		return nil
	},
}

func uninstallMemory(home, configDir string, confirmedRevoked bool) error {
	cfg, err := memory.Load(configDir)
	if err != nil {
		return fmt.Errorf("load memory config: %w", err)
	}
	if cfg != nil && cfg.APIKey != "" && !confirmedRevoked {
		return errors.New("refusing to orphan an active memory bearer: revoke the corresponding Memory key in AceTeam API Keys, then rerun with --confirmed-revoked")
	}
	if _, err := memory.RemoveMCPServer(memory.ClaudeJSONPath(home), memory.MCPServerName); err != nil {
		return fmt.Errorf("remove memory MCP server: %w", err)
	}
	settings := memory.ClaudeSettingsPath(home)
	if _, err := memory.RemoveHook(settings, "UserPromptSubmit", memory.RecallMarker); err != nil {
		return fmt.Errorf("remove recall hook: %w", err)
	}
	if _, err := memory.RemoveHook(settings, "SessionEnd", memory.CaptureMarker); err != nil {
		return fmt.Errorf("remove capture hook: %w", err)
	}
	if err := memory.Remove(configDir); err != nil {
		return fmt.Errorf("remove local memory credential: %w", err)
	}
	return nil
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
	pollCtx, cancelPoll := context.WithCancel(context.Background())
	defer cancelPoll()

	tokenChan := make(chan *nexus.MemoryTokenResponse, 1)
	errChan := make(chan error, 1)

	go func() {
		token, err := client.PollForMemoryTokenContext(pollCtx, resp.DeviceCode, resp.Interval)
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
	// Stop polling promptly when the UI was canceled. On terminal states the
	// buffered token/error was queued before the UI status update ended Run.
	cancelPoll()
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
	if hook.HookEventName != "" && hook.HookEventName != "UserPromptSubmit" {
		os.Exit(0)
	}

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

Designed to be run by Claude Code's SessionEnd hook. It captures a provided
--note or a bounded semantic selection of user/assistant text parsed from the
session transcript. It does not claim turn-by-turn capture.

Always exits 0 so it never disrupts Claude Code.`,
	Run: runMemoryCapture,
}

func runMemoryCapture(cmd *cobra.Command, args []string) {
	cfg, err := memory.Load(platform.ConfigDir())
	if err != nil || cfg == nil || cfg.APIKey == "" {
		os.Exit(0)
	}

	hook := readHookInput()
	if hook.HookEventName != "" && hook.HookEventName != "SessionEnd" {
		os.Exit(0)
	}

	note := memoryCaptureNote
	if note == "" && len(args) > 0 {
		note = strings.Join(args, " ")
	}
	if note == "" {
		// Best-effort: semantic text only. Raw transcript JSON, tool payloads,
		// thinking, and metadata are intentionally excluded.
		note = memory.TranscriptSummary(hook.TranscriptPath, 1500)
	}
	if strings.TrimSpace(note) == "" {
		// Nothing durable to record; exit quietly.
		os.Exit(0)
	}
	if err := memoryCaptureSecretCheck(note); err != nil {
		// Fail closed without echoing or rewriting the sensitive prose.
		fmt.Fprintf(os.Stderr, "citadel memory capture: %v\n", err)
		os.Exit(0)
	}

	name := memoryCaptureName
	if name == "" {
		name = memory.StableCaptureName(hook.SessionID, hook.TranscriptPath, note)
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

// quoteHookArg quotes one executable path for Claude Code's shell command
// hook. POSIX single quotes suppress command substitution and variable
// expansion. cmd.exe does not expand either construct inside a quoted path.
func quoteHookArg(arg string) (string, error) {
	return quoteHookArgForOS(runtime.GOOS, arg)
}

func quoteHookArgForOS(goos, arg string) (string, error) {
	if goos == "windows" {
		// cmd.exe expands %VAR% even inside double quotes, and may expand !VAR!
		// when delayed expansion is enabled. Refuse such executable paths rather
		// than installing a hook that changes meaning when it runs.
		if strings.ContainsAny(arg, "%!\r\n\"") {
			return "", errors.New("unsafe Windows hook executable path contains shell expansion characters")
		}
		return `"` + arg + `"`, nil
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'", nil
}

func memoryElevatedUserCheck(elevated bool, sudoUser string) error {
	if !elevated {
		return nil
	}
	if strings.TrimSpace(sudoUser) != "" && sudoUser != "root" {
		return fmt.Errorf("memory configuration is user-scoped; rerun without sudo as %s", sudoUser)
	}
	return errors.New("memory configuration is user-scoped; refusing elevated/root execution")
}

func memoryCaptureSecretCheck(note string) error {
	return memory.ValidateCaptureContent(note)
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
	memoryCmd.AddCommand(memoryUninstallCmd)

	memoryInstallCmd.Flags().BoolVar(&memoryInstallForce, "force", false, "Rotate the memory key (currently fails closed until backend revocation is available)")
	memoryUninstallCmd.Flags().BoolVar(&memoryUninstallConfirmed, "confirmed-revoked", false, "Confirm the remote Memory key was revoked before deleting its only local copy")

	memoryRecallCmd.Flags().StringVar(&memoryRecallScope, "scope", "", "Restrict search to a memory scope (project name or 'global'; default: all scopes)")
	memoryRecallCmd.Flags().StringVar(&memoryRecallQuery, "query", "", "Search query (default: the prompt from the hook stdin)")

	memoryCaptureCmd.Flags().StringVar(&memoryCaptureNote, "note", "", "Note text to capture (default: bounded semantic transcript text)")
	memoryCaptureCmd.Flags().StringVar(&memoryCaptureName, "name", "", "Memory slug (default: stable hash of the Claude session ID)")
	memoryCaptureCmd.Flags().StringVar(&memoryCaptureScope, "scope", "", "Memory scope to write (project name or 'global'; default: global)")
	memoryCaptureCmd.Flags().StringVar(&memoryCaptureDesc, "description", "", "Short description for the memory frontmatter")
}
