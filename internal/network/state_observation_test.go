package network

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func observationTestInputs(t *testing.T) stateObservationInputs {
	t.Helper()
	base := t.TempDir()
	return stateObservationInputs{
		globalDir:   filepath.Join(base, "machine"),
		goos:        "linux",
		getenv:      func(string) string { return "" },
		currentUser: func() (*user.User, error) { return &user.User{Username: "owner"}, nil },
		lookupUser:  func(string) (*user.User, error) { return &user.User{HomeDir: filepath.Join(base, "owner")}, nil },
		userHomeDir: func() (string, error) { return filepath.Join(base, "current"), nil },
		readFile:    func(string, int64) ([]byte, bool, error) { return nil, false, nil },
	}
}

func TestObserveStateDirLazyPrecedence(t *testing.T) {
	for _, source := range []string{"pointer", "machine", "owner", "current"} {
		t.Run(source, func(t *testing.T) {
			in := observationTestInputs(t)
			owner, _ := in.lookupUser("owner")
			current, _ := in.userHomeDir()
			paths := []string{
				filepath.Join(in.globalDir, machineStatePointerFile),
				filepath.Join(in.globalDir, "config.yaml"),
				filepath.Join(owner.HomeDir, ".citadel-cli", "config.yaml"),
				filepath.Join(current, ".citadel-cli", "config.yaml"),
			}
			selected := map[string]int{"pointer": 0, "machine": 1, "owner": 2, "current": 3}[source]
			nodeDir := filepath.Join(t.TempDir(), "node")
			reads := 0
			in.readFile = func(path string, limit int64) ([]byte, bool, error) {
				if reads > selected || path != paths[reads] {
					t.Fatalf("unexpected lower-priority read %q at index %d", path, reads)
				}
				index := reads
				reads++
				wantLimit := int64(stateObservationConfigLimit)
				if index == 0 {
					wantLimit = stateObservationPointerLimit
				}
				if limit != wantLimit {
					t.Errorf("limit = %d, want %d", limit, wantLimit)
				}
				if index != selected {
					return nil, false, nil
				}
				if index == 0 {
					return []byte("  " + nodeDir + "\n"), true, nil
				}
				return []byte(fmt.Sprintf("node_config_dir: %q\n", nodeDir)), true, nil
			}
			if selected <= 1 {
				in.currentUser = func() (*user.User, error) {
					t.Fatal("lower-priority identity lookup")
					return nil, nil
				}
				in.lookupUser = func(string) (*user.User, error) {
					t.Fatal("lower-priority owner lookup")
					return nil, nil
				}
			}
			if selected <= 2 {
				in.userHomeDir = func() (string, error) {
					t.Fatal("lower-priority current home lookup")
					return "", nil
				}
			}
			got, err := observeStateDir(in)
			want := resolveStateDir(stateDirInputs{pointerDir: nodeDir, goos: in.goos})
			if err != nil || got != want || reads != selected+1 {
				t.Fatalf("selection = %q, %v, reads %d; want %q and %d reads", got, err, reads, want, selected+1)
			}
		})
	}
}

func TestObserveStateDirFailureDoesNotFallBack(t *testing.T) {
	for _, failure := range []string{"pointer-read", "pointer-empty", "pointer-relative", "pointer-nul", "config-read", "config-yaml", "config-empty-path", "config-nonstring", "config-duplicate", "config-extra-document", "current-user", "owner-lookup", "owner-home", "current-home"} {
		t.Run(failure, func(t *testing.T) {
			in := observationTestInputs(t)
			reads := 0
			in.readFile = func(path string, _ int64) ([]byte, bool, error) {
				reads++
				if strings.HasPrefix(failure, "pointer-") && reads == 1 {
					switch failure {
					case "pointer-read":
						return nil, false, fmt.Errorf("PRIVATE_PATH_AND_TOKEN: %w", os.ErrPermission)
					case "pointer-relative":
						return []byte("relative"), true, nil
					case "pointer-nul":
						return []byte("/private\x00token"), true, nil
					default:
						return []byte(" \n"), true, nil
					}
				}
				if strings.HasPrefix(failure, "config-") && reads == 2 {
					bodies := map[string]string{
						"config-yaml":           "node_config_dir: [PRIVATE_TOKEN",
						"config-empty-path":     "node_config_dir: ''",
						"config-nonstring":      "node_config_dir: 42",
						"config-duplicate":      "node_config_dir: /one\nnode_config_dir: /two",
						"config-extra-document": "node_config_dir: /one\n---\nnode_config_dir: /two",
					}
					if failure == "config-read" {
						return nil, false, os.ErrPermission
					}
					return []byte(bodies[failure]), true, nil
				}
				if (strings.HasPrefix(failure, "pointer-") && reads > 1) || (strings.HasPrefix(failure, "config-") && reads > 2) {
					t.Fatal("continued to lower configuration after unknown input")
				}
				return nil, false, nil
			}
			switch failure {
			case "current-user":
				in.currentUser = func() (*user.User, error) { return nil, errors.New("PRIVATE_USER") }
			case "owner-lookup":
				in.lookupUser = func(string) (*user.User, error) { return nil, errors.New("PRIVATE_USER") }
			case "owner-home":
				in.lookupUser = func(string) (*user.User, error) { return &user.User{}, nil }
			case "current-home":
				in.userHomeDir = func() (string, error) { return "", errors.New("PRIVATE_HOME") }
			}
			got, err := observeStateDir(in)
			if got != "" || err != errStateObservation || err.Error() != "session state observation unavailable" || errors.Unwrap(err) != nil {
				t.Fatalf("unknown input did not produce only static error: %q, %v", got, err)
			}
		})
	}
}

func TestObserveStateDirOwnerAndLegacyParity(t *testing.T) {
	in := observationTestInputs(t)
	in.getenv = func(name string) string {
		if name == "SUDO_USER" {
			return "sudo-owner"
		}
		return ""
	}
	owner, _ := in.lookupUser("sudo-owner")
	in.currentUser = func() (*user.User, error) {
		t.Fatal("SUDO_USER must skip Current")
		return nil, nil
	}
	in.lookupUser = func(name string) (*user.User, error) {
		if name != "sudo-owner" {
			t.Fatalf("looked up %q instead of explicit owner", name)
		}
		return owner, nil
	}
	got, err := observeStateDir(in)
	want := resolveStateDir(stateDirInputs{ownerHome: owner.HomeDir, goos: in.goos})
	if err != nil || got != want {
		t.Fatalf("fresh/legacy fallback = %q, %v, want %q", got, err, want)
	}
}

func TestObserveStateDirWindowsLocalAppData(t *testing.T) {
	in := observationTestInputs(t)
	in.goos = "windows"
	appData := filepath.Join(t.TempDir(), "local-app-data")
	nodeDir := filepath.Join(t.TempDir(), "node")
	in.getenv = func(name string) string {
		if name == "LOCALAPPDATA" {
			return appData
		}
		return ""
	}
	in.currentUser = func() (*user.User, error) {
		t.Fatal("Windows must not lookup Unix owner")
		return nil, nil
	}
	in.lookupUser = func(string) (*user.User, error) {
		t.Fatal("Windows must not lookup Unix owner")
		return nil, nil
	}
	in.userHomeDir = func() (string, error) {
		t.Fatal("LOCALAPPDATA must precede home lookup")
		return "", nil
	}
	in.readFile = func(path string, _ int64) ([]byte, bool, error) {
		if path == filepath.Join(appData, "Citadel", "config.yaml") {
			return []byte(fmt.Sprintf("node_config_dir: %q", nodeDir)), true, nil
		}
		return nil, false, nil
	}
	got, err := observeStateDir(in)
	if err != nil || got != filepath.Join(nodeDir, "network") {
		t.Fatalf("Windows config = %q, %v", got, err)
	}
	in.readFile = func(string, int64) ([]byte, bool, error) {
		return []byte(`C:\Windows\System32\config\systemprofile\citadel-node`), true, nil
	}
	if _, err := observeStateDir(in); err != errStateObservation {
		t.Fatal("explicit inaccessible SYSTEM-profile selector must be unknown")
	}
}

func TestObserveConfigStateDirLegacyYAMLAliases(t *testing.T) {
	in := observationTestInputs(t)
	nodeDir := filepath.Join(t.TempDir(), "node")
	in.readFile = func(string, int64) ([]byte, bool, error) {
		return []byte(fmt.Sprintf("defaults: &defaults\n  node_config_dir: %q\n<<: *defaults\n", nodeDir)), true, nil
	}
	got, found, err := observeConfigStateDir(in, "unused")
	if err != nil || !found || got != filepath.Join(nodeDir, "network") {
		t.Fatalf("valid legacy YAML merge = %q, %t, %v", got, found, err)
	}
}

func TestValidObservedPathWindowsProfileSeparators(t *testing.T) {
	// Keep every fixture native-absolute on this CI host: a Windows drive
	// literal on Unix would fail IsAbs without exercising the profile policy.
	base := filepath.ToSlash(t.TempDir())
	for _, suffix := range []string{
		"/Windows/System32/config/systemprofile/node",
		`/Windows\System32\config\systemprofile\node`,
		`/Windows/System32\config/SYSTEMPROFILE\node`,
		"/Windows/System32/config/SyStEmPrOfIlE/node",
	} {
		path := base + suffix
		if !filepath.IsAbs(path) {
			t.Fatalf("fixture is not native-absolute: %q", path)
		}
		if validObservedPath(path, "windows") {
			t.Errorf("Windows profile alias accepted: %q", path)
		}
	}
	ordinary := filepath.Join(t.TempDir(), "ordinary", "node")
	if !validObservedPath(ordinary, "windows") {
		t.Fatal("ordinary absolute Windows policy path refused")
	}
	if !validObservedPath(base+"/systemprofile/node", "linux") {
		t.Fatal("non-Windows literal systemprofile path refused")
	}
}

func TestSessionControlEndpointAbsentIsNarrow(t *testing.T) {
	if !SessionControlEndpointAbsent(fmt.Errorf("transport: %w", os.ErrNotExist)) {
		t.Fatal("wrapped ENOENT must be absent")
	}
	for _, err := range []error{nil, os.ErrPermission, errors.New("file does not exist"), errors.New("connection refused"), errStateObservation} {
		if SessionControlEndpointAbsent(err) {
			t.Fatalf("non-absence error classified absent: %v", err)
		}
	}
}

func TestStateObservationPinsInitialIdentityBeforeOpen(t *testing.T) {
	// os.Stat's Windows FileInfo initializes its file ID lazily in SameFile.
	// Pin the actual source boundary on every CI host, not just Unix's eager
	// identity behavior. This is not native Windows execution evidence.
	file, err := parser.ParseFile(token.NewFileSet(), "state_observation.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "readStateObservationFile" && fn.Name.Name != "ObserveStatePresence") {
			continue
		}
		var statPos, pinPos, openPos token.Pos
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "openStateObservation" {
				openPos = call.Pos()
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			if selector.Sel.Name == "Stat" && statPos == token.NoPos {
				statPos = call.Pos()
			}
			if selector.Sel.Name == "SameFile" && len(call.Args) == 2 {
				first, firstOK := call.Args[0].(*ast.Ident)
				second, secondOK := call.Args[1].(*ast.Ident)
				if firstOK && secondOK && first.Name == "before" && second.Name == "before" {
					pinPos = call.Pos()
				}
			}
			return true
		})
		if statPos == token.NoPos || pinPos <= statPos || openPos <= pinPos {
			t.Errorf("%s must pin initial identity after Stat and before open", fn.Name.Name)
		}
		checked++
	}
	if checked != 2 {
		t.Fatalf("checked %d read-only entry points, want 2", checked)
	}
}
