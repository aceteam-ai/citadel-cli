// cmd/local_config.go
//
// `citadel local config <harness>`: print (and, with --write, merge) the MCP
// server entry that points an agent app at `citadel local mcp` (aceteam#13503
// S6). This file owns each harness's config location and shape. Update them
// here, not in the docs, when a harness moves its file. A location marked
// unverified is printed with that caveat.
//   - copilot: GitHub Copilot CLI, $COPILOT_HOME/mcp-config.json (default
//     ~/.copilot), {"mcpServers": {name: {"type": "local", "command", "args",
//     "tools": ["*"]}}}. VS Code's own mcp.json (a top-level "servers" key) is
//     a different file and is not written here.
//   - claude-desktop: %APPDATA%\Claude\claude_desktop_config.json (Windows),
//     ~/Library/Application Support/Claude/... (macOS); the Linux path is
//     unofficial. {"mcpServers": {name: {"command", "args"}}}
//   - opencode: ~/.config/opencode/opencode.json (or an existing opencode.jsonc),
//     {"mcp": {"servers": {name: {"type": "local", "command": [exe, "local",
//     "mcp"]}}}} per https://opencode.ai/docs/mcp-servers/ and
//     https://opencode.ai/docs/config/ (the "V2 (latest)" docs; the older
//     flat "mcp": {name: ...} shape is refused, not rewritten). The docs name
//     no env override and no Windows user path, so the Windows location is
//     marked unverified. A server connects unless "disabled": true.
//   - codex: $CODEX_HOME/config.toml (default ~/.codex),
//     [mcp_servers.<name>] command = ..., args = [...]
package cmd

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/spf13/cobra"
)

// localServerName is the key the entry is written under.
const localServerName = "citadel"

type harnessSpec struct {
	name   string
	title  string
	format string // "json" or "toml"
	// jsonKeys is the nested object path holding the servers (JSON only);
	// nil means ["mcpServers"].
	jsonKeys []string
	path     func(env harnessEnv) string
	entry    func(exe string) map[string]any
	// unverified explains why a location could not be confirmed for env, or "".
	unverified func(env harnessEnv) string
}

// harnessEnv is the injected view of the machine harness paths depend on.
type harnessEnv struct {
	goos       string
	home       string
	appData    string // %APPDATA% on Windows
	codexHome  string // $CODEX_HOME
	copilotDir string // $COPILOT_HOME
}

func systemHarnessEnv() harnessEnv {
	home, _ := platform.UserHomeDir()
	return harnessEnv{
		goos: runtime.GOOS, home: home, appData: os.Getenv("APPDATA"),
		codexHome: os.Getenv("CODEX_HOME"), copilotDir: os.Getenv("COPILOT_HOME"),
	}
}

var localArgs = []string{"local", "mcp"}

var harnesses = []harnessSpec{
	{
		name: "copilot", title: "GitHub Copilot CLI", format: "json",
		path: func(e harnessEnv) string {
			if e.copilotDir != "" {
				return filepath.Join(e.copilotDir, "mcp-config.json")
			}
			return filepath.Join(e.home, ".copilot", "mcp-config.json")
		},
		entry: func(exe string) map[string]any {
			return map[string]any{"type": "local", "command": exe, "args": localArgs, "tools": []string{"*"}}
		},
		unverified: func(harnessEnv) string { return "" },
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
		unverified: func(e harnessEnv) string {
			if e.goos != "windows" && e.goos != "darwin" {
				return "Claude Desktop has no official Linux build; this path is the community one"
			}
			return ""
		},
	},
	{
		name: "opencode", title: "opencode", format: "json",
		jsonKeys: []string{"mcp", "servers"},
		path: func(e harnessEnv) string {
			dir := filepath.Join(e.home, ".config", "opencode")
			// opencode accepts opencode.json or opencode.jsonc; keep using the
			// one that exists (json wins when both do).
			if _, err := os.Stat(filepath.Join(dir, "opencode.json")); err != nil {
				if _, err := os.Stat(filepath.Join(dir, "opencode.jsonc")); err == nil {
					return filepath.Join(dir, "opencode.jsonc")
				}
			}
			return filepath.Join(dir, "opencode.json")
		},
		entry: func(exe string) map[string]any {
			return map[string]any{"type": "local", "command": append([]string{exe}, localArgs...)}
		},
		unverified: func(e harnessEnv) string {
			if e.goos == "windows" {
				return "opencode's docs give ~/.config/opencode/ and no Windows user path; this assumes the same folder under your user profile"
			}
			return ""
		},
	},
	{
		name: "codex", title: "Codex", format: "toml",
		path: func(e harnessEnv) string {
			if e.codexHome != "" {
				return filepath.Join(e.codexHome, "config.toml")
			}
			return filepath.Join(e.home, ".codex", "config.toml")
		},
		entry:      func(exe string) map[string]any { return map[string]any{"command": exe, "args": localArgs} },
		unverified: func(harnessEnv) string { return "" },
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
	var doc any = map[string]any{localServerName: h.entry(exe)}
	keys := h.keyPath()
	for i := len(keys) - 1; i >= 0; i-- {
		doc = map[string]any{keys[i]: doc}
	}
	b, _ := marshalNoEscape(doc, "  ")
	return string(b) + "\n"
}

func (h harnessSpec) keyPath() []string {
	if len(h.jsonKeys) > 0 {
		return h.jsonKeys
	}
	return []string{"mcpServers"}
}

func marshalNoEscape(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

var errWriteDeclined = errors.New("nothing written")

// errAlreadyConfigured means the file already has exactly this entry.
var errAlreadyConfigured = errors.New("already configured")

// orderedObject is a JSON object kept as (key, raw value) pairs, so a merge
// leaves every other value byte-for-byte (numbers, escapes) and key order.
type orderedObject struct {
	keys []string
	vals []json.RawMessage
}

func parseOrderedObject(data []byte) (*orderedObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("top level is not a JSON object")
	}
	o := &orderedObject{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.keys = append(o.keys, key)
		o.vals = append(o.vals, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON object")
	}
	return o, nil
}

func (o *orderedObject) get(key string) (json.RawMessage, bool) {
	for i, k := range o.keys {
		if k == key {
			return o.vals[i], true
		}
	}
	return nil, false
}

func (o *orderedObject) set(key string, v json.RawMessage) {
	for i, k := range o.keys {
		if k == key {
			o.vals[i] = v
			return
		}
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, v)
}

func (o *orderedObject) marshal() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := marshalNoEscape(k, "")
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[i])
	}
	b.WriteByte('}')
	return b.Bytes()
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	return da.Decode(&x) == nil && db.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

// mergeJSON returns old with the citadel entry set under the nested object
// path keys, preserving every other key, value and order.
func mergeJSON(path string, old []byte, keys []string, want map[string]any, confirm func(string) bool) ([]byte, error) {
	root := &orderedObject{}
	if len(bytes.TrimSpace(old)) > 0 {
		var err error
		if root, err = parseOrderedObject(old); err != nil {
			return nil, fmt.Errorf("%s is not strict JSON (%v). Comments and trailing commas are not rewritten safely; paste the block by hand", path, err)
		}
	}
	// chain[i] is the object at keys[:i]; chain[0] is root.
	chain := []*orderedObject{root}
	for i, k := range keys {
		next := &orderedObject{}
		if raw, ok := chain[i].get(k); ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var err error
			if next, err = parseOrderedObject(raw); err != nil {
				return nil, fmt.Errorf("%s has a %q value that is not an object; paste the block by hand", path, k)
			}
		} else if k == "servers" && i > 0 && keys[i-1] == "mcp" && len(chain[i].keys) > 0 {
			// opencode's older flat layout ("mcp": {name: ...}) is a
			// different schema; adding "servers" beside real entries would
			// corrupt it.
			return nil, fmt.Errorf("%s uses an mcp layout without a \"servers\" object; add the entry by hand", path)
		}
		chain = append(chain, next)
	}
	servers := chain[len(keys)]
	wantRaw, err := marshalNoEscape(want, "")
	if err != nil {
		return nil, err
	}
	if cur, ok := servers.get(localServerName); ok {
		if jsonEqual(cur, wantRaw) {
			return nil, errAlreadyConfigured
		}
		if !confirm(fmt.Sprintf("%s already has a different %q server entry. Replace it?", path, localServerName)) {
			return nil, errWriteDeclined
		}
	}
	servers.set(localServerName, wantRaw)
	for i := len(keys) - 1; i >= 0; i-- {
		chain[i].set(keys[i], chain[i+1].marshal())
	}
	var out bytes.Buffer
	if err := json.Indent(&out, root.marshal(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// inlineMCPServers matches a top-level `mcp_servers = { ... }` assignment.
var inlineMCPServers = regexp.MustCompile(`(?m)^[ \t]*(mcp_servers|"mcp_servers"|'mcp_servers')[ \t]*=`)

// mergeTOML parses the Codex config, so an existing citadel server in any TOML
// form is found. An identical entry is a no-op; a different one is refused
// (the file is not rewritten, so its comments stay); otherwise the table is
// appended and the result must still parse.
func mergeTOML(path string, old []byte, h harnessSpec, exe string) ([]byte, error) {
	var doc map[string]any
	if _, err := toml.Decode(string(old), &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid TOML (%v); fix it or paste the block by hand", path, err)
	}
	if servers, ok := doc["mcp_servers"].(map[string]any); ok {
		if _, has := servers[localServerName]; !has && inlineMCPServers.Match(old) {
			// TOML inline tables are closed; appending [mcp_servers.citadel]
			// would be invalid for Codex's parser even though some accept it.
			return nil, fmt.Errorf("%s defines mcp_servers as an inline table, which cannot be extended; add the entry inside it by hand", path)
		}
		if cur, ok := servers[localServerName].(map[string]any); ok {
			cmd, _ := cur["command"].(string)
			if cmd == exe && reflect.DeepEqual(stringList(cur["args"]), localArgs) {
				return nil, errAlreadyConfigured
			}
			return nil, fmt.Errorf("%s already defines mcp_servers.%s differently; edit it to match the block above", path, localServerName)
		}
	} else if _, exists := doc["mcp_servers"]; exists {
		return nil, fmt.Errorf("%s has an mcp_servers value that is not a table; paste the block by hand", path)
	}
	next := append([]byte{}, old...)
	if len(next) > 0 && !bytes.HasSuffix(next, []byte("\n")) {
		next = append(next, '\n')
	}
	if len(next) > 0 {
		next = append(next, '\n')
	}
	next = append(next, renderBlock(h, exe)...)
	var check map[string]any
	if _, err := toml.Decode(string(next), &check); err != nil {
		return nil, fmt.Errorf("adding the entry to %s would make it invalid TOML (%v); paste the block by hand", path, err)
	}
	return next, nil
}

func stringList(v any) []string {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeExclusive creates path (which must not exist) with data and mode.
func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// mergeHarnessConfig writes the entry into the harness file, keeping every
// other setting. A symlinked config (dotfile managers) is written through to
// its target, and the file's mode is kept. A backup of the previous file is
// kept beside it. It returns the path written, or errAlreadyConfigured.
func mergeHarnessConfig(h harnessSpec, path, exe string, confirm func(string) bool, now time.Time) (string, error) {
	target := path
	mode := os.FileMode(0o600)
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return "", fmt.Errorf("%s is a symlink whose target cannot be resolved (%v); fix the link or paste the block by hand", path, err)
			}
			target = resolved
			if fi, err = os.Stat(target); err != nil {
				return "", err
			}
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file; paste the block by hand", path)
		}
		mode = fi.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	old, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", target, err)
	}
	var next []byte
	if h.format == "toml" {
		next, err = mergeTOML(path, old, h, exe)
	} else {
		next, err = mergeJSON(path, old, h.keyPath(), h.entry(exe), confirm)
	}
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	if len(old) > 0 {
		backup := target + ".bak-" + now.UTC().Format("20060102T150405Z")
		if err := writeExclusive(backup, old, mode); err != nil {
			backup = backup + "-" + randSuffix()
			if err := writeExclusive(backup, old, mode); err != nil {
				return "", fmt.Errorf("back up %s: %w", target, err)
			}
		}
	}
	tmp := target + ".citadel-tmp-" + randSuffix()
	if err := writeExclusive(tmp, next, mode); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return target, nil
}

// citadelExecutable is the absolute path of this binary, so a GUI-launched
// harness that does not see an updated PATH can still start it. A Homebrew
// install gets its stable bin symlink, not the versioned Cellar path a
// `brew upgrade` deletes.
func citadelExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if runtime.GOOS == "darwin" {
		if stable, ok := update.StableDarwinExecPath(exe); ok {
			return stable, nil
		}
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
	fmt.Fprintf(out, "%s reads its MCP servers from:\n  %s\n", h.title, path)
	if note := h.unverified(env); note != "" {
		fmt.Fprintf(out, "  (unverified: %s)\n", note)
	}
	if h.name == "copilot" {
		fmt.Fprintln(out, "  (VS Code keeps its own MCP servers in mcp.json under a \"servers\" key; this file is the Copilot CLI's.)")
	}
	fmt.Fprintf(out, "\nAdd this entry:\n\n%s\n", renderBlock(h, exe))
	if !write {
		fmt.Fprintf(out, "Then restart %s. To have Citadel merge it for you (with a backup): %s local config %s --write\n", h.title, quoteExe(exe), h.name)
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
	switch {
	case errors.Is(err, errAlreadyConfigured):
		fmt.Fprintf(out, "%s already has this entry; nothing to change.\n", path)
		return nil
	case errors.Is(err, errWriteDeclined):
		fmt.Fprintln(out, "Nothing written.")
		return nil
	case err != nil:
		return err
	}
	fmt.Fprintf(out, "Wrote %s. Restart %s to load it.\n", written, h.title)
	return nil
}

// quoteExe renders the executable for a command the user can paste.
func quoteExe(exe string) string {
	if strings.ContainsAny(exe, " \t") {
		return "& \"" + exe + "\""
	}
	return exe
}

// harnessConfigStatus is one harness's state for `citadel local doctor`.
type harnessConfigStatus struct {
	Harness string
	Path    string
	Found   bool
	// Points is true when the file has a server entry whose command is an
	// existing executable and whose args are exactly ["local", "mcp"].
	Points bool
	// Command is that entry's command (for the doctor line).
	Command string
}

// entryPoints reports whether a server entry runs `<existing exe> local mcp`.
func entryPoints(command string, args []string, exists func(string) bool) bool {
	return command != "" && reflect.DeepEqual(args, localArgs) && exists(command)
}

// detectHarnessConfigs reports which harness config files exist and whether
// one of their servers runs `citadel local mcp` from an existing executable.
func detectHarnessConfigs(env harnessEnv) []harnessConfigStatus {
	exists := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && fi.Mode().IsRegular()
	}
	var out []harnessConfigStatus
	for _, h := range harnesses {
		st := harnessConfigStatus{Harness: h.name, Path: h.path(env)}
		data, err := os.ReadFile(st.Path)
		if err != nil {
			out = append(out, st)
			continue
		}
		st.Found = true
		var servers map[string]any
		if h.format == "toml" {
			var doc map[string]any
			if _, err := toml.Decode(string(data), &doc); err == nil {
				servers, _ = doc["mcp_servers"].(map[string]any)
			}
		} else {
			var doc map[string]any
			if json.Unmarshal(data, &doc) == nil {
				cur := doc
				for _, k := range h.keyPath() {
					cur, _ = cur[k].(map[string]any)
				}
				servers = cur
			}
		}
		for _, v := range servers {
			e, _ := v.(map[string]any)
			cmd, _ := e["command"].(string)
			args := stringList(e["args"])
			if list, ok := e["command"].([]any); ok && len(list) > 0 {
				// opencode: command is [exe, args...].
				all := stringList(list)
				cmd, args = all[0], all[1:]
			}
			if entryPoints(cmd, args, exists) {
				st.Points, st.Command = true, cmd
				break
			}
		}
		out = append(out, st)
	}
	return out
}
