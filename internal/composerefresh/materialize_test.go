package composerefresh_test

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	"github.com/aceteam-ai/citadel-cli/services"
)

func TestUnknownGPUFreshAndExistingCompose(t *testing.T) {
	for _, name := range []string{"ollama", "llamacpp"} {
		for _, flavor := range []string{"missing", "cpu", "gpu", "custom"} {
			t.Run(name+"/"+flavor, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, name+".yml")
				var before string
				switch flavor {
				case "cpu":
					before, _ = services.ComposeForHost(name, "linux", func() int { return 0 })
				case "gpu":
					before = services.ServiceMap[name]
				case "custom":
					before = "operator-owned compose\n"
				}
				if flavor != "missing" {
					if err := os.WriteFile(path, []byte(before), 0600); err != nil {
						t.Fatal(err)
					}
				}
				probes := 0
				refresh, missing := services.ComposeRefreshPlan("linux", func() int { probes++; return -1 })
				if probes != 1 {
					t.Fatalf("probe count %d", probes)
				}
				if _, ok := refresh[name]; ok {
					t.Fatal("uncertain existing file offered for refresh")
				}
				created, err := composerefresh.MaterializeMissing(dir, missing)
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if flavor == "missing" {
					if string(got) != services.ServiceMap[name] || !services.ComposeDeclaresNvidiaReservation(string(got)) {
						t.Fatal("missing file did not get default NVIDIA template")
					}
					if len(created) != 2 {
						t.Fatalf("fresh directory created %v", created)
					}
					if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
						t.Fatalf("file mode: %v %v", info, err)
					}
				} else if string(got) != before {
					t.Fatal("existing compose was changed")
				}
				// The same-version sweep is allowed to skip; creation already completed.
				opts := composerefresh.Options{ServicesDir: dir, Version: "fixture-v1", Embedded: refresh}
				if _, err := composerefresh.Sweep(opts); err != nil {
					t.Fatal(err)
				}
				if result, err := composerefresh.Sweep(opts); err != nil || !result.Skipped {
					t.Fatalf("same-version fast path changed: %+v %v", result, err)
				}
				// Deletion of a materialized file is retried before the next fast path.
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if _, err := composerefresh.MaterializeMissing(dir, missing); err != nil {
					t.Fatal(err)
				}
				got, err = os.ReadFile(path)
				if err != nil || string(got) != services.ServiceMap[name] {
					t.Fatal("same-version absence did not recover", err)
				}
			})
		}
	}
}

func TestMaterializeMissingPreservesSymlinkAndTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permission is host-dependent on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("owned target"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ollama.yml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	created, err := composerefresh.MaterializeMissing(dir, map[string]string{"ollama": "default"})
	if err != nil || len(created) != 0 {
		t.Fatal(created, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "owned target" {
		t.Fatal("symlink target changed", err)
	}
	if link, err := os.Readlink(path); err != nil || link != target {
		t.Fatal("symlink replaced", err)
	}
}

func TestMaterializeMissingConcurrentCreatorsDoNotReplace(t *testing.T) {
	dir := t.TempDir()
	var created atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names, err := composerefresh.MaterializeMissing(dir, map[string]string{"ollama": "complete compose"})
			if err != nil {
				t.Error(err)
				return
			}
			created.Add(int64(len(names)))
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created %d times", created.Load())
	}
	got, err := os.ReadFile(filepath.Join(dir, "ollama.yml"))
	if err != nil || string(got) != "complete compose" {
		t.Fatal(string(got), err)
	}
}

func TestMaterializeMissingRefusesBadInputsBeforeWriting(t *testing.T) {
	for _, name := range []string{"../escape", "", ".hidden", "a/b"} {
		dir := filepath.Join(t.TempDir(), "services")
		if _, err := composerefresh.MaterializeMissing(dir, map[string]string{name: "compose"}); err == nil {
			t.Fatal("invalid name accepted", name)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("directory created before input refusal")
		}
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := composerefresh.MaterializeMissing(filepath.Join(blocked, "services"), map[string]string{"ollama": "compose"}); err == nil {
		t.Fatal("write failure hidden")
	}
}
