//go:build linux

package jobs

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

// prePinTEI rebuilds the TEI template shipped before the model revision pin and
// checks it is the registered historical hash.
func prePinTEI(t *testing.T) string {
	t.Helper()
	old := strings.Replace(embeddedservices.ServiceMap["tei"], `    # The model revision is pinned. Upstream's 2026-11-21 config change lists a
    # token-classification architecture without id2label, which TEI 1.6 refuses
    # to load, so an unpinned "main" crash-loops on every fresh start.
`, "", 1)
	old = strings.Replace(old, `, "--revision", "9bbca17d9273fd0d03d5725c7a4b0f6b45142062"`, "", 1)
	if !embeddedservices.KnownComposeHashes["tei"][fmt.Sprintf("%x", sha256.Sum256([]byte(old)))] {
		t.Fatal("pre-pin TEI template is not a registered historical hash")
	}
	return old
}

// A SERVICE_START for a running TEI whose compose is the pre-pin citadel
// template refreshes the file and recreates the container exactly once. The
// next start takes the already-running shortcut, an operator-edited file is
// never recreated, and the opt-out keeps the shortcut. The container engine is a
// fake script on an isolated PATH that only records its arguments.
func TestServiceStartRecreatesRunningTEIOnceAfterTemplateRefresh(t *testing.T) {
	old := prePinTEI(t)
	for _, tc := range []struct {
		name     string
		compose  string
		optOut   bool
		wantUps  int
		wantFile string
	}{
		{name: "pre-pin template", compose: old, wantUps: 1, wantFile: embeddedservices.ServiceMap["tei"]},
		{name: "operator edited", compose: old + "\n# operator change\n", wantFile: old + "\n# operator change\n"},
		{name: "opted out", compose: old, optOut: true, wantFile: embeddedservices.ServiceMap["tei"]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir, write := fakeBinDir(t)
			logPath := filepath.Join(binDir, "calls.log")
			write("docker", fmt.Sprintf("echo \"$*\" >> %q\nexit 0\n", logPath))
			t.Setenv("CITADEL_NODE_DIR", "")
			t.Setenv("CITADEL_CONTAINER_RUNTIME", "docker")
			t.Setenv("CITADEL_RESOURCE_ISOLATION", "")
			t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "")
			if tc.optOut {
				t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "1")
			}
			dir := t.TempDir()
			h := NewServiceHandler(dir)
			h.dockerServiceRunningFn = func(string) bool { return true }
			h.externalEngineServingFn = func(context.Context, string, int) ([]string, bool) { return nil, false }
			if err := os.MkdirAll(filepath.Join(dir, "services"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "services", "tei.yml")
			if err := os.WriteFile(path, []byte(tc.compose), 0600); err != nil {
				t.Fatal(err)
			}
			svc := manifestService{Name: "tei", Type: "docker", ComposeFile: "services/tei.yml"}
			for i := 0; i < 2; i++ {
				out, err := h.serviceStart(testCtx(), svc, "", 0, 0, trustRemoteCodeUnspecified)
				if err != nil || strings.Contains(string(out), `"error"`) {
					t.Fatalf("start %d = %s, %v", i, out, err)
				}
			}
			calls, _ := os.ReadFile(logPath)
			ups := 0
			for _, line := range strings.Split(string(calls), "\n") {
				if strings.Contains(line, "compose") && strings.Contains(line, " up -d") {
					ups++
					if !strings.Contains(line, path) {
						t.Fatalf("compose up did not use the refreshed file: %s", line)
					}
				}
			}
			if ups != tc.wantUps {
				t.Fatalf("compose ups = %d, want %d; calls:\n%s", ups, tc.wantUps, calls)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.wantFile {
				t.Fatalf("compose after start:\n%s", got)
			}
		})
	}
}

// A failed recreate restores the pre-pin file so the next start retries.
func TestServiceStartTemplateRecreateFailureRestoresPreviousFile(t *testing.T) {
	old := prePinTEI(t)
	_, write := fakeBinDir(t)
	write("docker", "case \"$1\" in compose) echo boom; exit 1;; esac\nexit 0\n")
	t.Setenv("CITADEL_NODE_DIR", "")
	t.Setenv("CITADEL_CONTAINER_RUNTIME", "docker")
	t.Setenv("CITADEL_RESOURCE_ISOLATION", "")
	t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "")
	dir := t.TempDir()
	h := NewServiceHandler(dir)
	h.dockerServiceRunningFn = func(string) bool { return true }
	h.externalEngineServingFn = func(context.Context, string, int) ([]string, bool) { return nil, false }
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "services", "tei.yml")
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := h.serviceStart(testCtx(), manifestService{Name: "tei", Type: "docker", ComposeFile: "services/tei.yml"}, "", 0, 0, trustRemoteCodeUnspecified)
	if err != nil || !strings.Contains(string(out), "boom") {
		t.Fatalf("start = %s, %v; want the compose failure reported", out, err)
	}
	if got, _ := os.ReadFile(path); string(got) != old {
		t.Fatalf("pre-pin file not restored:\n%s", got)
	}
}
