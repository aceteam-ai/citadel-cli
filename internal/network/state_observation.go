package network

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// These are read-only observation limits, not limits imposed on legacy state
// readers or writers. An oversized or indeterminate input refuses observation.
const (
	stateObservationPointerLimit = 16 << 10
	stateObservationConfigLimit  = 1 << 20
)

var errStateObservation = errors.New("session state observation unavailable")

// ObserveStateDir selects the existing state location without creating,
// repairing, or changing permissions. Unlike GetStateDir, an unreadable or
// explicitly invalid higher-priority input is unknown, not permission to fall
// back to a different node identity. Valid higher inputs skip all lower reads.
func ObserveStateDir() (string, error) {
	return observeStateDir(stateObservationInputs{
		globalDir:   getGlobalConfigDirForState(),
		goos:        runtime.GOOS,
		getenv:      os.Getenv,
		currentUser: user.Current,
		lookupUser:  user.Lookup,
		userHomeDir: os.UserHomeDir,
		readFile:    readStateObservationFile,
	})
}

type stateObservationInputs struct {
	globalDir   string
	goos        string
	getenv      func(string) string
	currentUser func() (*user.User, error)
	lookupUser  func(string) (*user.User, error)
	userHomeDir func() (string, error)
	readFile    func(string, int64) ([]byte, bool, error)
}

func observeStateDir(in stateObservationInputs) (string, error) {
	if !filepath.IsAbs(in.globalDir) {
		return "", errStateObservation
	}
	data, exists, err := in.readFile(filepath.Join(in.globalDir, machineStatePointerFile), stateObservationPointerLimit)
	if err != nil {
		return "", errStateObservation
	}
	if exists {
		dir := strings.TrimSpace(string(data))
		return observedNodeStateDir(dir, in.goos)
	}
	if dir, found, err := observeConfigStateDir(in, filepath.Join(in.globalDir, "config.yaml")); err != nil || found {
		return dir, err
	}

	// Match candidateOwnerHomes/getOwnerHomeDir's owner-first decisions, but do
	// not swallow Current/Lookup failures and continue with the invoker's HOME.
	ownerHome := ""
	if in.goos != "windows" {
		owner := in.getenv("SUDO_USER")
		if owner == "" {
			u, err := in.currentUser()
			if err != nil || u == nil || u.Username == "" {
				return "", errStateObservation
			}
			owner = u.Username
		}
		u, err := in.lookupUser(owner)
		if err != nil || u == nil || !validObservedPath(u.HomeDir, in.goos) {
			return "", errStateObservation
		}
		ownerHome = u.HomeDir
		if dir, found, err := observeConfigStateDir(in, filepath.Join(ownerHome, ".citadel-cli", "config.yaml")); err != nil || found {
			return dir, err
		}
	}
	currentHome, err := observedCurrentHome(in)
	if err != nil {
		return "", err
	}
	if currentHome != ownerHome {
		if dir, found, err := observeConfigStateDir(in, filepath.Join(currentHome, ".citadel-cli", "config.yaml")); err != nil || found {
			return dir, err
		}
	}
	if in.goos == "windows" {
		if appData := in.getenv("LOCALAPPDATA"); appData != "" {
			if !validObservedPath(appData, in.goos) {
				return "", errStateObservation
			}
			if dir, found, err := observeConfigStateDir(in, filepath.Join(appData, "Citadel", "config.yaml")); err != nil || found {
				return dir, err
			}
		}
		ownerHome = currentHome
	}
	// Legacy populated and fresh fallback have the same location. Presence is
	// deliberately observed later, after endpoint absence and lock observation.
	return resolveStateDir(stateDirInputs{ownerHome: ownerHome, goos: in.goos}), nil
}

func observedCurrentHome(in stateObservationInputs) (string, error) {
	if in.goos == "windows" {
		for _, name := range []string{"LOCALAPPDATA", "APPDATA"} {
			if home := in.getenv(name); home != "" {
				if !validObservedPath(home, in.goos) {
					return "", errStateObservation
				}
				return home, nil
			}
		}
	}
	home, err := in.userHomeDir()
	if err != nil || !validObservedPath(home, in.goos) {
		return "", errStateObservation
	}
	return home, nil
}

func validObservedPath(path, goos string) bool {
	policyPath := path
	if goos == "windows" {
		// Native Windows accepts both separators. Normalize only the policy
		// view so the legacy backslash guard also sees forward/mixed aliases.
		policyPath = strings.ReplaceAll(policyPath, "/", `\`)
	}
	return utf8.ValidString(path) && !strings.ContainsRune(path, 0) &&
		filepath.IsAbs(path) && usableNodeDir(policyPath, goos) != ""
}

func observedNodeStateDir(dir, goos string) (string, error) {
	if !validObservedPath(dir, goos) {
		return "", errStateObservation
	}
	return resolveStateDir(stateDirInputs{pointerDir: dir, goos: goos}), nil
}

func observeConfigStateDir(in stateObservationInputs, path string) (string, bool, error) {
	data, exists, err := in.readFile(path, stateObservationConfigLimit)
	if err != nil {
		return "", false, errStateObservation
	}
	if !exists {
		return "", false, nil
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if !utf8.Valid(data) {
		return "", false, errStateObservation
	}
	if err := decoder.Decode(&doc); err != nil && err != io.EOF {
		return "", false, errStateObservation
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF {
		return "", false, errStateObservation
	}
	if len(doc.Content) == 0 {
		return "", false, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return "", false, errStateObservation
	}
	// Decode the legacy shape as well: duplicate keys, malformed mappings and
	// invalid field types must not become an apparently absent selector.
	var legacy struct {
		NodeConfigDir string `yaml:"node_config_dir"`
	}
	if root.Decode(&legacy) != nil {
		return "", false, errStateObservation
	}
	var fields map[string]any
	if root.Decode(&fields) != nil {
		return "", false, errStateObservation
	}
	if value, found := fields["node_config_dir"]; found {
		if _, ok := value.(string); !ok {
			return "", false, errStateObservation
		}
		dir, err := observedNodeStateDir(legacy.NodeConfigDir, in.goos)
		return dir, true, err
	}
	return "", false, nil
}

// readStateObservationFile follows existing trusted aliases, but never reads a
// FIFO/device. Descriptor identity and metadata must remain stable through the
// bounded read. This is a filesystem snapshot, not a launch/ownership lock;
// synchronous filesystem operations have no claimed hard deadline.
func readStateObservationFile(path string, limit int64) ([]byte, bool, error) {
	before, err := os.Stat(path)
	if err != nil {
		if observedPathAbsent(path, err) {
			return nil, false, nil
		}
		return nil, false, errStateObservation
	}
	// Windows Stat defers file-ID lookup until SameFile. Pin that identity
	// before open, not afterward at a potentially replaced pathname.
	if !before.Mode().IsRegular() || before.Size() > limit || !os.SameFile(before, before) {
		return nil, false, errStateObservation
	}
	f, err := openStateObservation(path)
	if err != nil {
		return nil, false, errStateObservation
	}
	data, readErr := readStateObservationFD(f, path, before, limit)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return nil, false, errStateObservation
	}
	return data, true, nil
}

func readStateObservationFD(f *os.File, path string, before os.FileInfo, limit int64) ([]byte, error) {
	opened, err := f.Stat()
	if err != nil || !sameStateObservation(before, opened) {
		return nil, errStateObservation
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errStateObservation
	}
	afterFD, fdErr := f.Stat()
	afterPath, pathErr := os.Stat(path)
	if fdErr != nil || pathErr != nil || !sameStateObservation(opened, afterFD) || !sameStateObservation(opened, afterPath) {
		return nil, errStateObservation
	}
	return data, nil
}

func sameStateObservation(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() &&
		a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// ENOENT through a dangling ancestor is unknown. Only a missing suffix below
// an existing directory is confirmed absent; this never creates that suffix.
func observedPathAbsent(path string, statErr error) bool {
	if !errors.Is(statErr, os.ErrNotExist) {
		return false
	}
	for candidate := filepath.Clean(path); ; candidate = filepath.Dir(candidate) {
		info, err := os.Lstat(candidate)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				info, err = os.Stat(candidate)
			}
			return err == nil && candidate != filepath.Clean(path) && info.IsDir()
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(candidate) == candidate {
			return false
		}
	}
}

// ObserveStatePresence is HasState's any-entry predicate with an explicit
// unknown result. It observes only the supplied selected directory; it does
// not re-resolve identity, create state, or validate credentials.
func ObserveStatePresence(stateDir string) (bool, error) {
	if !validObservedPath(stateDir, runtime.GOOS) {
		return false, errStateObservation
	}
	before, err := os.Stat(stateDir)
	if err != nil {
		if observedPathAbsent(stateDir, err) {
			return false, nil
		}
		return false, errStateObservation
	}
	// As above, eagerly resolve Windows' lazy file ID before opening the path.
	if !before.IsDir() || !os.SameFile(before, before) {
		return false, errStateObservation
	}
	f, err := openStateObservation(stateDir)
	if err != nil {
		return false, errStateObservation
	}
	opened, statErr := f.Stat()
	var entries []string
	var readErr error
	if statErr == nil && opened.IsDir() && os.SameFile(before, opened) {
		entries, readErr = f.Readdirnames(1)
	} else {
		statErr = errStateObservation
	}
	afterFD, fdErr := f.Stat()
	afterPath, pathErr := os.Stat(stateDir)
	closeErr := f.Close()
	if statErr != nil || (readErr != nil && readErr != io.EOF) || fdErr != nil || pathErr != nil || closeErr != nil ||
		!sameStateObservation(opened, afterFD) || !sameStateObservation(opened, afterPath) {
		return false, errStateObservation
	}
	return len(entries) > 0, nil
}

// SessionControlEndpointAbsent is only for a failed request with no response.
// Protocol/status/permission/timeout failures never authorize another owner.
func SessionControlEndpointAbsent(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) || stateObservationConnectionRefused(err))
}
