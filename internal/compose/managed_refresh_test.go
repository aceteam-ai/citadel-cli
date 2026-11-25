package compose

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// prePinTEITemplate rebuilds the TEI template shipped before the model revision
// pin and checks it is the registered historical hash, so these tests exercise
// the real upgrade the pin needs.
func prePinTEITemplate(t *testing.T) string {
	t.Helper()
	current := services.ServiceMap["tei"]
	old := strings.Replace(current, `    # The model revision is pinned. Upstream's 2026-11-21 config change lists a
    # token-classification architecture without id2label, which TEI 1.6 refuses
    # to load, so an unpinned "main" crash-loops on every fresh start.
`, "", 1)
	old = strings.Replace(old, `, "--revision", "9bbca17d9273fd0d03d5725c7a4b0f6b45142062"`, "", 1)
	const want = "9292268ccf61886bbba4aad8bcb04963dfccc33d32e7b69eb8958f05bd3ee563"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(old))); got != want || !services.KnownComposeHashes["tei"][want] {
		t.Fatalf("pre-pin TEI template hash = %s, want registered %s", got, want)
	}
	return old
}

func TestRefreshKnownComposeOnDiskUpgradesKnownTemplateOnce(t *testing.T) {
	for _, namespaced := range []bool{false, true} {
		t.Run(fmt.Sprintf("namespaced=%v", namespaced), func(t *testing.T) {
			dir := t.TempDir()
			override := ""
			if namespaced {
				override = dir
			}
			path := filepath.Join(dir, "tei.yml")
			old, _, err := EnsureNamespacedContainerName(prePinTEITemplate(t), "tei", override)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(old), 0600); err != nil {
				t.Fatal(err)
			}
			if !IsKnownCompose(path, "tei", override) {
				t.Fatal("pre-pin template not recognized as citadel's")
			}
			var refresh StartRefresh
			refresh, err = CaptureStartRefresh(path, func() error {
				_, err := RefreshKnownComposeOnDisk(path, "tei", services.ServiceMap["tei"], override)
				return err
			})
			if err != nil || !refresh.Changed || refresh.Previous != old {
				t.Fatalf("refresh = %+v, %v; want a change from the pre-pin file", refresh, err)
			}
			want, _, _ := EnsureNamespacedContainerName(services.ServiceMap["tei"], "tei", override)
			if got, _ := os.ReadFile(path); string(got) != want || !strings.Contains(string(got), "--revision") {
				t.Fatalf("file not refreshed to the pinned template:\n%s", got)
			}
			if !IsKnownCompose(path, "tei", override) {
				t.Fatal("current template not recognized as citadel's")
			}
			again, err := CaptureStartRefresh(path, func() error {
				_, err := RefreshKnownComposeOnDisk(path, "tei", services.ServiceMap["tei"], override)
				return err
			})
			if err != nil || again.Changed {
				t.Fatalf("second refresh = %+v, %v; want no change", again, err)
			}
			if err := refresh.Rollback(path, "tei"); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(path); string(got) != old {
				t.Fatal("rollback did not restore the previous file")
			}
		})
	}
}

func TestRefreshKnownComposeOnDiskLeavesOperatorFilesAlone(t *testing.T) {
	old := prePinTEITemplate(t)
	for name, fixture := range map[string]string{
		"edited":            old + "\n# operator change\n",
		"foreign-namespace": rewriteHostVariantNameForTest(t, old, "tei", "/foreign"),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tei.yml")
			if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
				t.Fatal(err)
			}
			changed, err := RefreshKnownComposeOnDisk(path, "tei", services.ServiceMap["tei"], dir)
			if err != nil || changed {
				t.Fatalf("operator file changed: %v %v", changed, err)
			}
			if IsKnownCompose(path, "tei", dir) {
				t.Fatal("operator file reported as citadel's")
			}
			if got, _ := os.ReadFile(path); string(got) != fixture {
				t.Fatal("operator content lost")
			}
		})
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "operator.yml")
	path := filepath.Join(dir, "tei.yml")
	if err := os.WriteFile(target, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if changed, err := RefreshKnownComposeOnDisk(path, "tei", services.ServiceMap["tei"], ""); err != nil || changed {
		t.Fatalf("symlinked compose changed: %v %v", changed, err)
	}
	if got, _ := os.ReadFile(target); string(got) != old {
		t.Fatal("symlink target changed")
	}
	missing := filepath.Join(dir, "absent.yml")
	refresh, err := CaptureStartRefresh(missing, func() error { return os.WriteFile(missing, []byte(old), 0600) })
	if err != nil || refresh.Changed {
		t.Fatalf("first materialization reported as a change: %+v %v", refresh, err)
	}
}
