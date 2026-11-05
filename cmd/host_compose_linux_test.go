//go:build linux

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	"github.com/aceteam-ai/citadel-cli/services"
)

// These cmd tests are for CI; local verification does not execute the cmd suite.
func TestEnsureComposeFileSelectsHostVariantOnRepeatedCalls(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	for _, name := range []string{"ollama", "llamacpp"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			count := 0
			probe := func() int { return count }
			for _, n := range []int{0, 1, 0} {
				count = n
				if err := ensureComposeFileForHost(dir, name, "linux", probe); err != nil {
					t.Fatal(err)
				}
				want, _ := services.ComposeForHost(name, "linux", probe)
				got, err := os.ReadFile(filepath.Join(dir, "services", name+".yml"))
				if err != nil || string(got) != want {
					t.Fatalf("wrong host variant: %v", err)
				}
			}
		})
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "services", "custom.yml")
	if err := os.WriteFile(path, []byte("custom compose"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureComposeFileForHost(dir, "custom", "linux", func() int { t.Fatal("custom compose probed GPU"); return 0 }); err != nil {
		t.Fatal(err)
	}
}

func TestManifestStartReconcilesCPUVariantWithoutCreatingCustomFiles(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "services", "llamacpp.yml")
	if err := os.WriteFile(path, []byte(services.ServiceMap["llamacpp"]), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reconcileHostComposeForStart("llamacpp", path, func() int { return 0 }); err != nil {
		t.Fatal(err)
	}
	want, _ := services.ComposeForHost("llamacpp", "linux", func() int { return 0 })
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("manifest start kept GPU: %v", err)
	}
	if err := reconcileHostComposeForStart("llamacpp", filepath.Join(dir, "custom.yml"), func() int { t.Fatal("custom path probed GPU"); return 0 }); err != nil {
		t.Fatal(err)
	}
}

func TestBootRefreshUsesSelectedHostMap(t *testing.T) {
	dir := writeManifestWithServices(t, nil)
	t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "1")
	called := false
	refreshManagedComposeFilesWithSweep(dir, func(opts composerefresh.Options) (composerefresh.Result, error) {
		called = true
		for _, name := range []string{"ollama", "llamacpp"} {
			want, _ := services.ComposeForHost(name, "linux", func() int { return 0 })
			if opts.Embedded[name] != want {
				t.Fatalf("boot sweep kept GPU template for %s", name)
			}
		}
		if opts.Embedded["vllm"] != services.ServiceMap["vllm"] {
			t.Fatal("CUDA-only template changed")
		}
		return composerefresh.Result{}, nil
	}, func() int { return 0 })
	if !called {
		t.Fatal("boot sweep not invoked")
	}
}

func TestUnknownGPUKeepsBothExistingFlavorsAndSkipsBootRewrite(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	for _, name := range []string{"ollama", "llamacpp"} {
		for _, count := range []int{0, 1} {
			dir := t.TempDir()
			if err := ensureComposeFileForHost(dir, name, "linux", func() int { return count }); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "services", name+".yml")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := ensureComposeFileForHost(dir, name, "linux", func() int { return -1 }); err != nil {
				t.Fatal(err)
			}
			if err := reconcileHostComposeForStart(name, path, func() int { return -1 }); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("unknown GPU observation changed existing flavor", err)
			}
		}
	}
	dir := writeManifestWithServices(t, nil)
	t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "1")
	called := false
	refreshManagedComposeFilesWithSweep(dir, func(opts composerefresh.Options) (composerefresh.Result, error) {
		called = true
		for _, name := range []string{"ollama", "llamacpp"} {
			if _, ok := opts.Embedded[name]; ok {
				t.Fatal("unknown flavor was offered to sweep", name)
			}
		}
		return composerefresh.Result{}, nil
	}, func() int { return -1 })
	if !called {
		t.Fatal("sweep seam was not called")
	}
}
