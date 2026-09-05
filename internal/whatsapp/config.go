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
//
// The write is ATOMIC (tempfile in the same dir + os.Rename), because the
// bridge compose interpolates ADMIN_API_KEY with `${ADMIN_API_KEY:?}` and a
// truncated/partial env (a crash mid-write) would make every compose
// invocation -- including `down` -- fail to parse, bricking the bridge. A
// reader (or a concurrent `docker compose`) therefore always observes either
// the complete previous file or the complete new one, never a half-written
// one. This matters most for admin-key rotation (citadel#624 part 3), which
// rewrites this file out from under a running bridge.
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
	return writeFileAtomic0600(EnvPath(servicesDir), []byte(b.String()))
}

// writeFileAtomic0600 writes data to path atomically at mode 0600: it writes to
// a temp file in the SAME directory (so the final os.Rename is a same-filesystem
// atomic replace, not a cross-device copy), then renames it over path. On any
// error before the rename the temp file is removed, so a failed write never
// leaves a partial file at path -- either the old file survives untouched or,
// on the very first write, nothing is created. 0600 because the content holds
// the admin secret.
func writeFileAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp env file: %w", err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp env file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp env file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp env file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp env file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp env file into place: %w", err)
	}
	renamed = true
	return nil
}

// IsDeployed reports whether the bridge compose file has been materialized in
// the services directory (i.e. the module has been deployed at least once).
func IsDeployed(servicesDir string) bool {
	_, err := os.Stat(ComposePath(servicesDir))
	return err == nil
}
