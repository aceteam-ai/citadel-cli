package declaredsession

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// These new implementation ceilings bound source preparation. They are not
// hardware measurements or additional product decisions in the #1165 DoR.
const (
	MaxConfigBytes = 1 << 20
	MaxSessions    = 64
	MaxEnvBytes    = 64 << 10
	MaxStateBytes  = 64 << 10
	MaxEventBytes  = 16 << 10
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateName matches the canonical lease grammar in AceTeam mcp_sessions.py.
// Unlike header normalization it never trims, folds case or creates aliases.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return ErrConfig
	}
	return nil
}

func defaultDeclaration() Declaration {
	return Declaration{
		Mode:            "interactive",
		Enabled:         true,
		Restart:         RestartPolicy{Policy: "on-failure", MinSeconds: 1, MaxSeconds: 300, ResetAfter: 600},
		Attach:          AttachPolicy{Local: "interactive", Web: "readonly", MaxViewers: 4},
		ScrollbackBytes: 262144,
		Log:             LogPolicy{RotateBytes: 16777216, Keep: 5},
		AceTeam:         AceTeamPolicy{Register: true},
	}
}

// DecodeConfig validates the whole file before returning a usable snapshot.
// Callers retain their previous snapshot on errors; this function stops nothing.
func DecodeConfig(raw []byte) (Snapshot, error) {
	if len(raw) == 0 || len(raw) > MaxConfigBytes {
		return Snapshot{}, ErrConfig
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if decoder.Decode(&doc) != nil || len(doc.Content) != 1 || decoder.Decode(new(yaml.Node)) != io.EOF {
		return Snapshot{}, ErrConfig
	}
	root := doc.Content[0]
	if !mappingKeys(root, "sessions") || len(root.Content) != 2 {
		return Snapshot{}, ErrConfig
	}
	list := root.Content[1]
	if list.Kind != yaml.SequenceNode || list.Tag != "!!seq" || len(list.Content) > MaxSessions {
		return Snapshot{}, ErrConfig
	}
	snapshot := Snapshot{FileRevision: digest(raw), Sessions: make([]Declaration, 0, len(list.Content))}
	seen := map[string]bool{}
	for _, node := range list.Content {
		if !declarationNode(node) {
			return Snapshot{}, ErrConfig
		}
		d := defaultDeclaration()
		if node.Decode(&d) != nil || ValidateDeclaration(d) != nil || seen[d.Name] {
			return Snapshot{}, ErrConfig
		}
		seen[d.Name] = true
		// Revision excludes whitespace/key order and other declarations. It
		// includes all declared values, never the contents of the env_file.
		encoded, err := json.Marshal(d)
		if err != nil {
			return Snapshot{}, ErrConfig
		}
		d.Revision = digest(encoded)
		snapshot.Sessions = append(snapshot.Sessions, d)
	}
	return snapshot, nil
}

func mappingKeys(node *yaml.Node, allowed ...string) bool {
	if node.Kind != yaml.MappingNode || node.Tag != "!!map" || len(node.Content)%2 != 0 {
		return false
	}
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] || !oneOf(key.Value, allowed...) {
			return false
		}
		seen[key.Value] = true
	}
	return true
}

func declarationNode(node *yaml.Node) bool {
	if !mappingKeys(node, "name", "harness", "mode", "command", "command_restart", "cwd", "path_prepend", "env", "env_file", "restart", "attach", "scrollback_bytes", "log", "aceteam", "enabled") {
		return false
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "name", "harness", "mode", "cwd", "env_file":
			if !scalar(value, "!!str") {
				return false
			}
		case "command", "command_restart", "path_prepend":
			if value.Kind != yaml.SequenceNode || value.Tag != "!!seq" {
				return false
			}
			for _, item := range value.Content {
				if !scalar(item, "!!str") {
					return false
				}
			}
		case "env":
			if value.Kind != yaml.MappingNode || value.Tag != "!!map" {
				return false
			}
			seen := map[string]bool{}
			for j := 0; j < len(value.Content); j += 2 {
				k, v := value.Content[j], value.Content[j+1]
				if !scalar(k, "!!str") || !scalar(v, "!!str") || seen[k.Value] {
					return false
				}
				seen[k.Value] = true
			}
		case "restart":
			if !typedMap(value, map[string]string{"policy": "!!str", "min_s": "!!int", "max_s": "!!int", "reset_after_s": "!!int"}) {
				return false
			}
		case "attach":
			if !typedMap(value, map[string]string{"local": "!!str", "web": "!!str", "max_viewers": "!!int"}) {
				return false
			}
		case "log":
			if !typedMap(value, map[string]string{"rotate_bytes": "!!int", "keep": "!!int"}) {
				return false
			}
		case "aceteam":
			if !typedMap(value, map[string]string{"register": "!!bool", "takeover": "!!bool"}) {
				return false
			}
		case "enabled":
			if !scalar(value, "!!bool") {
				return false
			}
		case "scrollback_bytes":
			if !scalar(value, "!!int") {
				return false
			}
		}
	}
	return true
}

func typedMap(node *yaml.Node, types map[string]string) bool {
	keys := make([]string, 0, len(types))
	for key := range types {
		keys = append(keys, key)
	}
	if !mappingKeys(node, keys...) {
		return false
	}
	for i := 0; i < len(node.Content); i += 2 {
		if !scalar(node.Content[i+1], types[node.Content[i].Value]) {
			return false
		}
	}
	return true
}

func scalar(node *yaml.Node, tag string) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == tag
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func ValidateDeclaration(d Declaration) error {
	if ValidateName(d.Name) != nil || !oneOf(d.Harness, "claude-code", "codex", "ace-cli", "other") || !oneOf(d.Mode, "interactive", "headless") || !validPath(d.Cwd, false) || !validArgv(d.Command) {
		return ErrConfig
	}
	if d.CommandRestart != nil && !validArgv(d.CommandRestart) {
		return ErrConfig
	}
	if len(d.PathPrepend) > 32 || (d.EnvFile != "" && !validPath(d.EnvFile, true)) {
		return ErrConfig
	}
	for _, path := range d.PathPrepend {
		if !validPath(path, true) || strings.ContainsRune(path, ':') {
			return ErrConfig
		}
	}
	if !validEnv(d.Env) || !oneOf(d.Restart.Policy, "always", "on-failure", "never") || d.Restart.MinSeconds < 1 || d.Restart.MaxSeconds < d.Restart.MinSeconds || d.Restart.MaxSeconds > 3600 || d.Restart.ResetAfter < 1 || d.Restart.ResetAfter > 86400 {
		return ErrConfig
	}
	if !oneOf(d.Attach.Local, "interactive", "readonly", "deny") || !oneOf(d.Attach.Web, "interactive", "readonly", "deny") || d.Attach.MaxViewers < 1 || d.Attach.MaxViewers > 32 || d.ScrollbackBytes < 0 || d.ScrollbackBytes > 4<<20 || validateLogPolicy(d.Log) != nil {
		return ErrConfig
	}
	return nil
}

func validArgv(argv []string) bool {
	if len(argv) == 0 || len(argv) > 64 || strings.TrimSpace(argv[0]) == "" {
		return false
	}
	total := 0
	for _, arg := range argv {
		total += len(arg)
		if len(arg) > 8192 || strings.ContainsRune(arg, 0) {
			return false
		}
	}
	return total <= 64<<10
}

func validPath(path string, tilde bool) bool {
	return path != "" && len(path) <= 4096 && !strings.ContainsRune(path, 0) && (filepath.IsAbs(path) || (tilde && (path == "~" || strings.HasPrefix(path, "~/"))))
}

func validEnv(env map[string]string) bool {
	if len(env) > 128 {
		return false
	}
	total := 0
	for key, value := range env {
		total += len(key) + len(value) + 2
		if !envPattern.MatchString(key) || reservedEnv(key) || strings.ContainsRune(value, 0) {
			return false
		}
	}
	return total <= MaxEnvBytes
}

func reservedEnv(key string) bool {
	return oneOf(key, "PATH", "HOME", "USER", "LOGNAME", "ACETEAM_SESSION", "ACETEAM_MACHINE", "ACETEAM_API_KEY", "ACETEAM_API_TOKEN", "CITADEL_API_KEY", "CITADEL_DEVICE_TOKEN", "CITADEL_DEVICE_API_TOKEN", "ACETEAM_DEVICE_TOKEN", "ACETEAM_DEVICE_API_TOKEN", "INTERNAL_AUTH_SECRET", "TS_AUTHKEY", "TAILSCALE_AUTHKEY", "HEADSCALE_PREAUTHKEY")
}

func validateLogPolicy(policy LogPolicy) error {
	if policy.RotateBytes < 4096 || policy.RotateBytes > 64<<20 || policy.Keep < 1 || policy.Keep > 16 {
		return ErrConfig
	}
	return nil
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
