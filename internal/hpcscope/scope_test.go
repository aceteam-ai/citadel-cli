//go:build !windows

package hpcscope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/clilog"
	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
)

const helperEnv = "CITADEL_HPCSCOPE_TEST_HELPER"

// resolvedPaths is what one simulated allocation reports back.
type resolvedPaths struct {
	Root            string   `json:"root"`
	Paths           []string `json:"paths"`
	KeyPEM          string   `json:"key_pem"`
	LegacyDisabled  bool     `json:"legacy_disabled"`
	SecondInstallOK bool     `json:"second_install_ok"`
	EnvHome         string   `json:"env_home"`
}

// TestHelperProcess is not a real test. It is the body of one simulated
// allocation, run in a child process so the one-shot process scope is
// installed exactly as `citadel --profile hpc` installs it.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process only")
	}
	out, err := runAllocation()
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: %v\n", err)
		os.Exit(3)
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func runAllocation() (resolvedPaths, error) {
	layout, err := hpc.ResolveLayout(hpc.Inputs{
		JobID:       os.Getenv(hpc.EnvJobID),
		TmpDir:      os.Getenv(hpc.EnvTmpDir),
		LegacyJobID: os.Getenv(hpc.EnvLegacyJobID),
	})
	if err != nil {
		return resolvedPaths{}, err
	}
	if err := hpc.Prepare(layout, os.Getuid()); err != nil {
		return resolvedPaths{}, err
	}
	if err := Install(layout); err != nil {
		return resolvedPaths{}, err
	}
	under := func(p string) bool { return p == layout.Root || strings.HasPrefix(p, layout.Root+"/") }

	homeAny, _ := platform.HomeDir("")
	homeRoot, _ := platform.HomeDir("root")
	nodeDefault, _ := platform.DefaultNodeDir("")
	paths := []string{
		platform.ConfigDir(),
		homeAny,
		homeRoot,
		nodeDefault,
		network.GetStateDir(),
		network.GetNodeConfigDir(),
		network.GetStatePath("tailscaled.state"),
		clilog.Dir(),
		worklock.LockPathForStateDir(network.GetStateDir()),
	}
	paths = append(paths, platform.ConfigDirCandidates()...)

	// Never write unless every resolved path is inside this job's root: a
	// broken scope must fail the test, not touch the host's real node state.
	for _, p := range paths {
		if !under(p) {
			return resolvedPaths{Root: layout.Root, Paths: paths}, nil
		}
	}
	store := nodeidentity.Convergent(network.GetNodeConfigDir())
	if !under(store.KeyPath()) {
		return resolvedPaths{Root: layout.Root, Paths: append(paths, store.KeyPath())}, nil
	}
	paths = append(paths, store.KeyPath())
	if _, err := store.GetOrCreateKey(); err != nil {
		return resolvedPaths{}, err
	}
	key, err := os.ReadFile(store.KeyPath())
	if err != nil {
		return resolvedPaths{}, err
	}

	other := layout
	other.ConfigDir = layout.HomeDir
	secondErr := Install(other)

	return resolvedPaths{
		Root:            layout.Root,
		Paths:           paths,
		KeyPEM:          string(key),
		LegacyDisabled:  nodeidentity.LegacyReadThroughDisabled(),
		SecondInstallOK: secondErr == nil,
		EnvHome:         os.Getenv("HOME"),
	}, nil
}

// seedSharedHome writes the state an ordinary citadel install leaves in a
// user's home: a config pointing at ~/citadel-node, existing network state,
// and a registered identity key. If the scope leaked, an allocation would
// resolve or adopt these.
func seedSharedHome(t *testing.T, home string) []byte {
	t.Helper()
	cli := filepath.Join(home, ".citadel-cli")
	node := filepath.Join(home, "citadel-node")
	for _, d := range []string{filepath.Join(cli, "identity"), filepath.Join(node, "network"), filepath.Join(node, "identity")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cli, "config.yaml"), []byte("node_config_dir: "+node+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "network", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := nodeidentity.New(filepath.Join(cli, "identity"))
	if _, err := legacy.GetOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(legacy.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "identity", "node.key"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestConcurrentAllocationsOnSharedHomeNeverSharePaths(t *testing.T) {
	home := t.TempDir()
	tmp := t.TempDir()
	legacyKey := seedSharedHome(t, home)

	jobs := []string{"5001", "5002"}
	results := make([]resolvedPaths, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
			cmd.Env = []string{
				helperEnv + "=1",
				"HOME=" + home,
				"TMPDIR=" + tmp,
				hpc.EnvJobID + "=" + job,
				"PATH=" + os.Getenv("PATH"),
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				errs[i] = fmt.Errorf("allocation %s: %v: %s", job, err, stderr.String())
				return
			}
			if err := json.Unmarshal(stdout.Bytes(), &results[i]); err != nil {
				errs[i] = fmt.Errorf("allocation %s: decode %q: %v", job, stdout.String(), err)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	forbidden := []string{filepath.Join(home, ".citadel-cli"), filepath.Join(home, "citadel-node"), "/etc/citadel", "/usr/local/etc/citadel"}
	for i, r := range results {
		if r.KeyPEM == "" {
			t.Fatalf("allocation %s resolved a path outside its job root: %v", jobs[i], r.Paths)
		}
		for _, p := range r.Paths {
			if !strings.HasPrefix(p, r.Root+"/") && p != r.Root {
				t.Fatalf("allocation %s: %s is outside %s", jobs[i], p, r.Root)
			}
			for _, f := range forbidden {
				if p == f || strings.HasPrefix(p, f+"/") {
					t.Fatalf("allocation %s resolved host path %s", jobs[i], p)
				}
			}
		}
		if r.KeyPEM == string(legacyKey) {
			t.Fatalf("allocation %s adopted the shared home's identity key", jobs[i])
		}
		if !r.LegacyDisabled {
			t.Fatalf("allocation %s left the legacy identity read-through on", jobs[i])
		}
		if r.SecondInstallOK {
			t.Fatalf("allocation %s allowed the scope to be replaced", jobs[i])
		}
		if !strings.HasPrefix(r.EnvHome, r.Root+"/") {
			t.Fatalf("allocation %s HOME=%s is outside its job root", jobs[i], r.EnvHome)
		}
	}
	if results[0].Root == results[1].Root {
		t.Fatal("two allocations share one job root")
	}
	for _, a := range results[0].Paths {
		for _, b := range results[1].Paths {
			if a == b {
				t.Fatalf("two allocations share %s", a)
			}
		}
	}
	if results[0].KeyPEM == results[1].KeyPEM {
		t.Fatal("two allocations share one identity key")
	}
}
