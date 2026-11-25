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

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/compose/composefake"
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

// teiStartFixture is a handler whose running-service replacement runs against a
// composefake engine; its other probes see a fake docker script on an isolated
// PATH that only exits 0.
func teiStartFixture(t *testing.T, composeText string, project string) (*ServiceHandler, *composefake.Engine, string, composefake.Container) {
	t.Helper()
	_, write := fakeBinDir(t)
	write("docker", "exit 0\n")
	t.Setenv("CITADEL_NODE_DIR", "")
	t.Setenv("CITADEL_CONTAINER_RUNTIME", "docker")
	t.Setenv("CITADEL_RESOURCE_ISOLATION", "")
	t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "")
	prevDelay := compose.ReplaceSettleDelay
	t.Cleanup(func() { compose.ReplaceSettleDelay = prevDelay })
	compose.ReplaceSettleDelay = 0
	dir := t.TempDir()
	h := NewServiceHandler(dir)
	engine := composefake.New("tei", "citadel-tei", "tei:pinned")
	h.composeExec = engine.Exec
	h.dockerServiceRunningFn = func(string) bool {
		c, ok := engine.Named("citadel-tei")
		return ok && c.State == "running"
	}
	h.externalEngineServingFn = func(context.Context, string, int) ([]string, bool) { return nil, false }
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "services", "tei.yml")
	if err := os.WriteFile(path, []byte(composeText), 0600); err != nil {
		t.Fatal(err)
	}
	old := *engine.Add(composefake.Container{Name: "citadel-tei", State: "running", Project: project, Service: "tei", Config: composefake.FileHash(path)})
	return h, engine, path, old
}

var teiSvc = manifestService{Name: "tei", Type: "docker", ComposeFile: "services/tei.yml"}

// A SERVICE_START for a running TEI whose compose is the pre-pin citadel
// template refreshes the file and replaces the container exactly once, with a
// plain up (a template-only change). The next start takes the already-running
// shortcut, an operator-edited file is never recreated, and the opt-out keeps
// the shortcut.
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
			h, engine, path, prev := teiStartFixture(t, tc.compose, "services")
			if tc.optOut {
				t.Setenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE", "1")
			}
			for i := 0; i < 2; i++ {
				out, err := h.serviceStart(testCtx(), teiSvc, "", 0, 0, trustRemoteCodeUnspecified)
				if err != nil || strings.Contains(string(out), `"error"`) {
					t.Fatalf("start %d = %s, %v", i, out, err)
				}
			}
			if ups := engine.Count("compose", "up", "-d"); ups != tc.wantUps || engine.Count("--force-recreate") != 0 {
				t.Fatalf("compose ups = %d (want %d, unforced): %v", ups, tc.wantUps, engine.Calls)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.wantFile {
				t.Fatalf("compose after start:\n%s", got)
			}
			c, _ := engine.Named("citadel-tei")
			if (c.ID != prev.ID) != (tc.wantUps == 1) || c.State != "running" {
				t.Fatalf("container = %+v (was %+v)", c, prev)
			}
		})
	}
}

// A failed replacement or preflight keeps the old container running under its
// own name and ID and restores the pre-pin file.
func TestServiceStartTemplateRecreateFailureKeepsOldContainer(t *testing.T) {
	old := prePinTEI(t)
	for _, step := range []string{"up-start", "config"} {
		t.Run(step, func(t *testing.T) {
			h, engine, path, prev := teiStartFixture(t, old, "citadel-tei")
			engine.Fail[step] = 1
			out, err := h.serviceStart(testCtx(), teiSvc, "", 0, 0, trustRemoteCodeUnspecified)
			if err != nil || !strings.Contains(string(out), `"error"`) {
				t.Fatalf("start = %s, %v; want the failure reported", out, err)
			}
			if got, _ := os.ReadFile(path); string(got) != old {
				t.Fatalf("pre-pin file not restored:\n%s", got)
			}
			all := engine.Containers()
			if len(all) != 1 || all[0] != prev {
				t.Fatalf("containers = %+v; want the old one untouched and running: %+v", all, prev)
			}
			if step == "config" && (engine.Count("stop") != 0 || engine.Count("rename") != 0 || engine.Count("up") != 0) {
				t.Fatalf("preflight failure touched the container: %v", engine.Calls)
			}
		})
	}
}
