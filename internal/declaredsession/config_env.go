package declaredsession

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type OwnerContext struct {
	UID      uint32
	Name     string
	Home     string
	Hostname string
}

type SpawnSpec struct {
	Argv            []string
	Cwd             string
	Env             []string
	EnvironmentHash string
}

// BuildSpawn has no ambient environment or device-config reads. The caller
// supplies verified owning-user context and only essential launcher variables.
// The returned Env contains secrets: never serialize it into logs/state/status.
// Absent command_restart reuses command; it invents no harness resume argument.
func (s *Store) BuildSpawn(d Declaration, restart bool, owner OwnerContext, essential map[string]string) (SpawnSpec, error) {
	if d.Mode != "interactive" {
		return SpawnSpec{}, ErrUnsupported
	}
	encoded, err := json.Marshal(d)
	if err != nil || ValidateDeclaration(d) != nil || !validDigest(d.Revision) || digest(encoded) != d.Revision || owner.UID != s.uid || owner.UID == 0 || !validPath(owner.Home, false) || owner.Name == "" || len(owner.Name) > 128 || owner.Hostname == "" || len(owner.Hostname) > 255 || strings.ContainsAny(owner.Name+owner.Hostname, "\x00\n\r") {
		return SpawnSpec{}, ErrConfig
	}
	env := map[string]string{}
	for key, value := range essential {
		if !oneOf(key, "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS") || strings.ContainsRune(value, 0) {
			return SpawnSpec{}, ErrConfig
		}
		env[key] = value
	}
	if !environmentBounded(env) {
		return SpawnSpec{}, ErrConfig
	}
	if d.EnvFile != "" {
		raw, err := readPrivatePath(expandHome(d.EnvFile, owner.Home), MaxEnvBytes, s.uid)
		if err != nil {
			return SpawnSpec{}, err
		}
		fileEnv, err := parseEnvFile(raw)
		if err != nil {
			return SpawnSpec{}, err
		}
		for key, value := range fileEnv {
			env[key] = value
		}
	}
	for key, value := range d.Env {
		env[key] = value
	}
	env["HOME"], env["USER"], env["LOGNAME"] = owner.Home, owner.Name, owner.Name
	env["ACETEAM_SESSION"], env["ACETEAM_MACHINE"] = d.Name, owner.Hostname
	if !environmentBounded(env) {
		return SpawnSpec{}, ErrConfig
	}
	paths := make([]string, 0, len(d.PathPrepend)+8)
	for _, path := range d.PathPrepend {
		paths = append(paths, expandHome(path, owner.Home))
	}
	for _, path := range filepath.SplitList(essential["PATH"]) {
		if !validPath(path, false) {
			return SpawnSpec{}, ErrConfig
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 || len(paths) > 32 {
		return SpawnSpec{}, ErrConfig
	}
	env["PATH"] = strings.Join(paths, string(os.PathListSeparator))
	argv := append([]string(nil), d.Command...)
	if restart && len(d.CommandRestart) != 0 {
		argv = append([]string(nil), d.CommandRestart...)
	}
	executable, err := resolveExecutable(argv[0], d.Cwd, paths)
	if err != nil {
		return SpawnSpec{}, err
	}
	argv[0] = executable
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	if !environmentBounded(env) {
		return SpawnSpec{}, ErrConfig
	}
	sort.Strings(keys)
	result := SpawnSpec{Argv: argv, Cwd: d.Cwd}
	for _, key := range keys {
		result.Env = append(result.Env, key+"="+env[key])
	}
	encoded, err = json.Marshal(result.Env)
	if err != nil {
		return SpawnSpec{}, ErrConfig
	}
	result.EnvironmentHash = digest(encoded)
	return result, nil
}

func environmentBounded(env map[string]string) bool {
	if len(env) > 128 {
		return false
	}
	total := 0
	for key, value := range env {
		total += len(key) + len(value) + 2
		if total > MaxEnvBytes {
			return false
		}
	}
	return true
}

// Deliberately literal dotenv subset: KEY=VALUE, blank lines and comments only.
// No export, quoting, interpolation or shell evaluation. Quotes are refused,
// rather than silently passing a quoted credential different from operator intent.
func parseEnvFile(raw []byte) (map[string]string, error) {
	if len(raw) > MaxEnvBytes || strings.ContainsRune(string(raw), 0) {
		return nil, ErrConfig
	}
	result := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if _, duplicate := result[key]; !found || duplicate || key != strings.TrimSpace(key) || strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			return nil, ErrConfig
		}
		result[key] = value
	}
	if !validEnv(result) {
		return nil, ErrConfig
	}
	return result, nil
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func resolveExecutable(command, cwd string, paths []string) (string, error) {
	candidates := []string{}
	if strings.ContainsRune(command, filepath.Separator) {
		if !filepath.IsAbs(command) {
			command = filepath.Join(cwd, command)
		}
		candidates = append(candidates, command)
	} else {
		for _, path := range paths {
			candidates = append(candidates, filepath.Join(path, command))
		}
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				return absolute, nil
			}
		}
	}
	return "", ErrUnavailable
}
