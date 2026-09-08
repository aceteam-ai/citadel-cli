// Package agentsprobe detects vendor coding-agent CLIs (Claude Code, Codex,
// Gemini CLI, OpenCode) already installed and authenticated on this node's
// own PATH, for its own account -- not a container or binary AceTeam ships.
//
// This is DoR-v2 slice S1 of issue #8993 (aceteam-ai/aceteam): "drive
// installed vendor coding agents on user hardware, wrapped in AEP receipts."
// S1 is discovery only -- it never drives, spawns, or drives a turn of any
// vendor agent, and it never makes a network call. Every check here is
// local PATH lookup, a bounded `--version` exec, and a local credential-file
// existence/shape check.
//
// Auth-state honesty: a vendor whose credential-file layout is not
// confidently known is reported as AuthStateUnknown rather than guessed at.
// A wrong guess in the "unauthenticated" direction is a false negative that
// looks like a clean "no"; reporting unknown keeps that distinction visible
// to callers instead of silently asserting something unverified.
package agentsprobe

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// probeTimeout bounds each vendor's --version exec. Mirrors the sibling
// internal/capabilities package's detectionTimeout.
const probeTimeout = 5 * time.Second

// AuthState is a tri-state auth signal. It is deliberately not a bool:
// collapsing "we could not tell" into "no" would misreport an authenticated
// vendor agent as unauthenticated whenever its credential file is merely
// unreadable or has an unexpected shape.
type AuthState string

const (
	// AuthStateAuthed means a credential file was found at the expected
	// path and parsed as a non-empty JSON object. This is a structural
	// signal only -- the token inside is never validated against the
	// vendor's servers (S1 makes no network call), so a stale or revoked
	// token still reads as authed here.
	AuthStateAuthed AuthState = "authed"
	// AuthStateNo means no credential file was found at the expected path.
	AuthStateNo AuthState = "unauthenticated"
	// AuthStateUnknown means auth state could not be determined: the
	// vendor's credential layout is not confidently known, the file
	// exists but could not be read (e.g. permission denied), or it exists
	// but did not parse as a JSON object.
	AuthStateUnknown AuthState = "unknown"
)

// VendorAgent is one probed vendor coding-agent CLI.
type VendorAgent struct {
	// Name is the stable vendor identifier (claude, codex, gemini, opencode).
	Name string `json:"name"`
	// Installed reports whether the binary was found on PATH.
	Installed bool `json:"installed"`
	// Version is the first line of `--version` output, best-effort parsed
	// down to a bare version number when one is found. Empty when the
	// binary is not installed or the version exec failed/timed out.
	Version string `json:"version,omitempty"`
	// Authed is the tri-state auth signal. Empty (omitted) when the
	// binary is not installed -- auth state is not meaningful for an
	// absent binary.
	Authed AuthState `json:"authed,omitempty"`
	// AdapterClass names the driving mechanism a future slice (S4/S5)
	// would use for this vendor: "claude-code-hooks" (headless print mode
	// + PreToolUse/PostToolUse/Stop hooks), "codex-exec-headless", or
	// "zed-acp" (Zed's Agent Client Protocol, JSON-RPC over stdio -- never
	// called bare "ACP" in this codebase; that name is reserved for
	// AceTeam's own Agent Compute Protocol). "unknown" when the mechanism
	// is not yet confidently mapped for this vendor.
	AdapterClass string `json:"adapter_class"`
}

type vendorSpec struct {
	name         string
	binary       string
	adapterClass string
	authCheck    func(homeDir string) AuthState
}

// vendorSpecs is the fixed set of vendors S1 probes for, per the DoR-v2
// discovery slice: "PATH plus auth-state detection per vendor (claude,
// codex, gemini, opencode)."
var vendorSpecs = []vendorSpec{
	{name: "claude", binary: "claude", adapterClass: "claude-code-hooks", authCheck: claudeAuthState},
	{name: "codex", binary: "codex", adapterClass: "codex-exec-headless", authCheck: codexAuthState},
	{name: "gemini", binary: "gemini", adapterClass: "zed-acp", authCheck: geminiAuthState},
	{name: "opencode", binary: "opencode", adapterClass: "unknown", authCheck: opencodeAuthState},
}

// Probe detects installed and authenticated vendor coding agents on this
// node. Read-only: it never executes an agent turn and never makes a
// network call -- only PATH lookup, a bounded `--version` exec per
// installed vendor, and a local credential-file presence/shape check.
func Probe(ctx context.Context) []VendorAgent {
	homeDir, _ := os.UserHomeDir()
	out := make([]VendorAgent, 0, len(vendorSpecs))
	for _, spec := range vendorSpecs {
		out = append(out, probeVendor(ctx, spec, homeDir))
	}
	return out
}

func probeVendor(ctx context.Context, spec vendorSpec, homeDir string) VendorAgent {
	agent := VendorAgent{Name: spec.name, AdapterClass: spec.adapterClass}

	path, err := exec.LookPath(spec.binary)
	if err != nil {
		return agent // Installed stays false; Authed stays "" (not meaningful).
	}
	agent.Installed = true
	agent.Version = probeVersion(ctx, path)

	if spec.authCheck == nil || homeDir == "" {
		agent.Authed = AuthStateUnknown
		return agent
	}
	agent.Authed = spec.authCheck(homeDir)
	return agent
}

// probeVersion runs `<path> --version` under probeTimeout and best-effort
// parses a version number out of the first line of output. Returns "" on
// any exec failure or timeout -- a missing version is not itself an error
// here, just an absent field.
func probeVersion(ctx context.Context, path string) string {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return parseVersion(string(out))
}

var versionNumberPattern = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// parseVersion extracts a bare version number (e.g. "1.2.3") from raw
// `--version` output, falling back to the trimmed first line when no
// version-shaped substring is found.
func parseVersion(raw string) string {
	firstLine := strings.TrimSpace(strings.SplitN(raw, "\n", 2)[0])
	if m := versionNumberPattern.FindString(firstLine); m != "" {
		return m
	}
	return firstLine
}

// jsonFileNonEmptyObjectState reports the auth-state signal implied by a
// vendor credential file's presence and shallow structural validity. It
// never reads, logs, or returns any field VALUE from the file -- only
// whether it exists and decodes into a JSON object with at least one key.
// This is a structural signal, not a validity check: an expired or revoked
// token still reads as AuthStateAuthed (S1 makes no network call to verify
// tokens against the vendor).
func jsonFileNonEmptyObjectState(path string) AuthState {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AuthStateNo
		}
		// Permission denied or another read error: ambiguous, never a
		// false "no".
		return AuthStateUnknown
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return AuthStateUnknown
	}
	if len(obj) == 0 {
		return AuthStateNo
	}
	return AuthStateAuthed
}

// claudeAuthState checks Claude Code's Linux credential file. On macOS,
// Claude Code may instead store credentials in the system keychain, which
// this file-based check cannot see -- a macOS node with keychain-only
// credentials reads as AuthStateNo here, a documented, narrow false
// negative, not a false "authed" claim.
func claudeAuthState(homeDir string) AuthState {
	return jsonFileNonEmptyObjectState(filepath.Join(homeDir, ".claude", ".credentials.json"))
}

// codexAuthState checks the Codex CLI's credential file.
func codexAuthState(homeDir string) AuthState {
	return jsonFileNonEmptyObjectState(filepath.Join(homeDir, ".codex", "auth.json"))
}

// geminiAuthState checks the Gemini CLI's OAuth credential file.
func geminiAuthState(homeDir string) AuthState {
	return jsonFileNonEmptyObjectState(filepath.Join(homeDir, ".gemini", "oauth_creds.json"))
}

// opencodeAuthState always reports AuthStateUnknown: OpenCode's credential
// file layout is not yet confidently known to this package. Guessing a path
// here risks a false AuthStateNo for a node that is, in fact, authenticated
// through a layout this function doesn't check -- see the package doc's
// auth-state honesty note.
func opencodeAuthState(string) AuthState {
	return AuthStateUnknown
}
