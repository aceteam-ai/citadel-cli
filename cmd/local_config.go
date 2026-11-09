// cmd/local_config.go
//
// `citadel local config <harness>`: print (and, with --write, merge) the MCP
// server entry that points an agent app at `citadel local mcp` (aceteam#13503
// S6). This file owns each harness's config location and shape. They were
// checked against each harness's own documentation for this release; update
// them here, not in the docs, when a harness moves its file:
//   - copilot: GitHub Copilot CLI, and VS Code's user-wide Copilot MCP file:
//     ~/.copilot/mcp-config.json, {"mcpServers": {name: {"type": "local",
//     "command", "args", "tools": ["*"]}}}
//   - claude-desktop: %APPDATA%\Claude\claude_desktop_config.json (Windows),
//     ~/Library/Application Support/Claude/... (macOS), ~/.config/Claude/...
//     (Linux), {"mcpServers": {name: {"command", "args"}}}
//   - codex: ~/.codex/config.toml, [mcp_servers.<name>] command = ..., args = [...]
package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// localServerName is the key the entry is written under.
const localServerName = "citadel"

type harnessSpec struct {
	name   string
	title  string
	format string // "json" or "toml"
	path   func(env harnessEnv) string
	entry  func(exe string) map[string]any
}

// harnessEnv is the injected view of the machine harness paths depend on.
type harnessEnv struct {
	goos    string
	home    string
	appData string // %APPDATA% on Windows
}

func systemHarnessEnv() harnessEnv {
	home, _ := os.UserHomeDir()
	return harnessEnv{goos: runtime.GOOS, home: home, appData: os.Getenv("APPDATA")}
}

var localArgs = []string{"local", "mcp"}

var harnesses = []harnessSpec{
	{
		name: "copilot", title: "GitHub Copilot (CLI, and VS Code's user-wide Copilot MCP file)", format: "json",
		path: func(e harnessEnv) string { return filepath.Join(e.home, ".copilot", "mcp-config.json") },
		entry: func(exe string) map[string]any {
			return map[string]any{"type": "local", "command": exe, "args": localArgs, "tools": []string{"*"}}
		},
	},
	{
		name: "claude-desktop", title: "Claude Desktop", format: "json",
		path: func(e harnessEnv) string {
			switch e.goos {
			case "windows":
				root := e.appData
				if root == "" {
					root = filepath.Join(e.home, "AppData", "Roaming")
				}
				return filepath.Join(root, "Claude", "claude_desktop_config.json")
			case "darwin":
				return filepath.Join(e.home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
			default:
				return filepath.Join(e.home, ".config", "Claude", "claude_desktop_config.json")
			}
		},
		entry: func(exe string) map[string]any { return map[string]any{"command": exe, "args": localArgs} },
	},
	{
		name: "codex", title: "Codex", format: "toml",
		path:  func(e harnessEnv) string { return filepath.Join(e.home, ".codex", "config.toml") },
		entry: func(exe string) map[string]any { return map[string]any{"command": exe, "args": localArgs} },
	},
}

func findHarness(name string) (harnessSpec, bool) {
	for _, h := range harnesses {
		if h.name == name {
			return h, true
		}
	}
	return harnessSpec{}, false
}

func harnessNames() []string {
	var n []string
	for _, h := range harnesses {
		n = append(n, h.name)
	}
	return n
}

// tomlString renders s as a TOML string: a literal string when possible (no
// escaping of Windows backslashes), otherwise a basic string.
func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// renderBlock is what the user pastes: the whole file shape for JSON harnesses,
// the table for Codex.
func renderBlock(h harnessSpec, exe string) string {
	if h.format == "toml" {
		args := make([]string, len(localArgs))
		for i, a := range localArgs {
			args[i] = tomlString(a)
		}
		return fmt.Sprintf("[mcp_servers.%s]\ncommand = %s\nargs = [%s]\n", localServerName, tomlString(exe), strings.Join(args, ", "))
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{localServerName: h.entry(exe)}}, "", "  ")
	return string(b) + "\n"
}

var errWriteDeclined = errors.New("nothing written")

// mergeHarnessConfig writes the entry into the harness file, keeping every other
// setting. JSON files are merged structurally; an existing different "citadel"
// entry is replaced only after confirmation. For Codex the table is appended
// when absent; an existing table is left alone (there is no TOML parser here).
// A backup of the previous file is kept beside it.
func mergeHarnessConfig(h harnessSpec, path, exe string, confirm func(string) bool, now time.Time) (string, error) {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var next []byte
	if h.format == "toml" {
		if bytes.Contains(old, []byte("[mcp_servers."+localServerName+"]")) {
			return "", fmt.Errorf("%s already has a [mcp_servers.%s] table; edit it to match the block above", path, localServerName)
		}
		next = append([]byte{}, old...)
		if len(next) > 0 && !bytes.HasSuffix(next, []byte("\n")) {
			next = append(next, '\n')
		}
		if len(next) > 0 {
			next = append(next, '\n')
		}
		next = append(next, renderBlock(h, exe)...)
	} else {
		doc := map[string]any{}
		if len(bytes.TrimSpace(old)) > 0 {
			if err := json.Unmarshal(old, &doc); err != nil {
				return "", fmt.Errorf("%s is not valid JSON (%v); fix it or paste the block by hand", path, err)
			}
		}
		servers, _ := doc["mcpServers"].(map[string]any)
		if doc["mcpServers"] != nil && servers == nil {
			return "", fmt.Errorf("%s has an mcpServers value that is not an object; paste the block by hand", path)
		}
		if servers == nil {
			servers = map[string]any{}
		}
		want := h.entry(exe)
		if cur, ok := servers[localServerName]; ok {
			curJSON, _ := json.Marshal(cur)
			wantJSON, _ := json.Marshal(want)
			if bytes.Equal(curJSON, wantJSON) {
				return path, nil
			}
			if !confirm(fmt.Sprintf("%s already has a %q server entry. Replace it?", path, localServerName)) {
				return "", errWriteDeclined
			}
		}
		servers[localServerName] = want
		doc["mcpServers"] = servers
		next, err = json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return "", err
		}
		next = append(next, '\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if len(old) > 0 {
		backup := path + ".bak-" + now.UTC().Format("20060102T150405Z")
		if err := os.WriteFile(backup, old, 0o600); err != nil {
			return "", fmt.Errorf("back up %s: %w", path, err)
		}
	}
	tmp := path + ".citadel-tmp"
	if err := os.WriteFile(tmp, next, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// citadelExecutable is the absolute, symlink-resolved path of this binary, so a
// GUI-launched harness that does not see an updated PATH can still start it.
func citadelExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

var (
	localConfigWrite bool
	localConfigYes   bool
)

var localConfigCmd = &cobra.Command{
	Use:       "config <" + strings.Join(harnessNames(), "|") + ">",
	Short:     "Print (or write) the MCP entry that connects an agent app to citadel local mcp",
	Args:      cobra.ExactArgs(1),
	ValidArgs: harnessNames(),
	RunE: func(cmd *cobra.Command, args []string) error {
		exe, err := citadelExecutable()
		if err != nil {
			return fmt.Errorf("find the citadel executable: %w", err)
		}
		return runLocalConfig(args[0], exe, systemHarnessEnv(), localConfigWrite, localConfigYes, cmd.InOrStdin(), cmd.OutOrStdout(), time.Now())
	},
}

func init() {
	localConfigCmd.Flags().BoolVar(&localConfigWrite, "write", false, "merge the entry into the harness's config file (asks first; keeps a backup)")
	localConfigCmd.Flags().BoolVarP(&localConfigYes, "yes", "y", false, "with --write, do not ask")
	localCmd.AddCommand(localConfigCmd)
}

func runLocalConfig(name, exe string, env harnessEnv, write, yes bool, in io.Reader, out io.Writer, now time.Time) error {
	h, ok := findHarness(name)
	if !ok {
		names := harnessNames()
		sort.Strings(names)
		return fmt.Errorf("unknown harness %q; choose one of: %s", name, strings.Join(names, ", "))
	}
	path := h.path(env)
	fmt.Fprintf(out, "%s reads its MCP servers from:\n  %s\n\nAdd this entry:\n\n%s\n", h.title, path, renderBlock(h, exe))
	if !write {
		fmt.Fprintf(out, "Then restart %s. To have Citadel merge it for you (with a backup): citadel local config %s --write\n", h.title, h.name)
		return nil
	}
	reader := bufio.NewReader(in)
	confirm := func(q string) bool {
		if yes {
			return true
		}
		fmt.Fprintf(out, "%s [y/N] ", q)
		line, _ := reader.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		return line == "y" || line == "yes"
	}
	if !confirm(fmt.Sprintf("Write this entry to %s?", path)) {
		fmt.Fprintln(out, "Nothing written.")
		return nil
	}
	written, err := mergeHarnessConfig(h, path, exe, confirm, now)
	if errors.Is(err, errWriteDeclined) {
		fmt.Fprintln(out, "Nothing written.")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s. Restart %s to load it.\n", written, h.title)
	return nil
}

// harnessConfigStatus is one harness's state for `citadel local doctor`.
type harnessConfigStatus struct {
	Harness string
	Path    string
	Found   bool
	// Points is true when the file has a server entry running `local mcp`.
	Points bool
}

// detectHarnessConfigs reports which harness config files exist and whether
// they already point at `citadel local mcp`.
func detectHarnessConfigs(env harnessEnv) []harnessConfigStatus {
	var out []harnessConfigStatus
	for _, h := range harnesses {
		st := harnessConfigStatus{Harness: h.name, Path: h.path(env)}
		data, err := os.ReadFile(st.Path)
		if err == nil {
			st.Found = true
			s := string(data)
			st.Points = strings.Contains(s, "citadel") && strings.Contains(s, `"local"`) && strings.Contains(s, `"mcp"`) ||
				(h.format == "toml" && strings.Contains(s, "[mcp_servers.") && strings.Contains(s, "'local'") && strings.Contains(s, "'mcp'"))
		}
		out = append(out, st)
	}
	return out
}
