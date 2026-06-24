package whatsapp

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EnvPath returns the absolute path of the bridge env file inside a node's
// services directory.
func EnvPath(servicesDir string) string {
	return filepath.Join(servicesDir, EnvFileName)
}

// ComposePath returns the absolute path of the bridge compose file inside a
// node's services directory.
func ComposePath(servicesDir string) string {
	return filepath.Join(servicesDir, ServiceName+".yml")
}

// LoadEnv reads the bridge env file into a key=value map. A missing file
// returns an empty map and no error (the bridge has simply not been deployed
// yet). Lines that are blank or start with '#' are ignored.
func LoadEnv(servicesDir string) (map[string]string, error) {
	path := EnvPath(servicesDir)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveEnv writes a key=value map to the bridge env file with 0600 permissions
// (it holds the admin secret). Keys are sorted for stable output.
func SaveEnv(servicesDir string, env map[string]string) error {
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return fmt.Errorf("create services dir: %w", err)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# whatsapp-bridge module config (written by `citadel whatsapp`).\n")
	b.WriteString("# Contains the admin secret -- keep this file private (0600).\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return os.WriteFile(EnvPath(servicesDir), []byte(b.String()), 0600)
}

// IsDeployed reports whether the bridge compose file has been materialized in
// the services directory (i.e. the module has been deployed at least once).
func IsDeployed(servicesDir string) bool {
	_, err := os.Stat(ComposePath(servicesDir))
	return err == nil
}
