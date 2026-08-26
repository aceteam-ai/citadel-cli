// internal/jobs/nodedir_test.go
//
// citadel#860: ensureEmbeddedComposeFile / embeddedContainerNameFor mirror
// cmd.ensureComposeFile / cmd.embeddedContainerName for this package, which
// cannot import cmd. These pin that the two derivations agree via the shared
// internal/compose helpers, using CITADEL_NODE_DIR (the only override signal
// this package can see) rather than the cmd-level --node-dir flag.
package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

func TestEmbeddedContainerNameFor_NoOverrideIsUnchanged(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	if got := embeddedContainerNameFor("vllm"); got != "citadel-vllm" {
		t.Fatalf("embeddedContainerNameFor(vllm) = %q, want citadel-vllm", got)
	}
}

func TestEmbeddedContainerNameFor_WithOverrideIsNamespaced(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CITADEL_NODE_DIR", dir)

	want := compose.ContainerName("vllm", dir)
	if got := embeddedContainerNameFor("vllm"); got != want {
		t.Fatalf("embeddedContainerNameFor(vllm) = %q, want %q", got, want)
	}
	if got := embeddedContainerNameFor("vllm"); got == "citadel-vllm" {
		t.Fatalf("expected an override-namespaced name, got the plain one: %q", got)
	}
}

// TestEmbeddedContainerNameFor_NonEmbeddedNameUnaffected pins the scope
// boundary: a name that is not a services.ServiceMap entry (e.g. a
// claudecode instance's ContainerName, containerNamePrefix in
// service_payload.go) always keeps the plain "citadel-<name>" convention,
// even under an active override.
func TestEmbeddedContainerNameFor_NonEmbeddedNameUnaffected(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", t.TempDir())
	if got := embeddedContainerNameFor("some-instance-name"); got != "citadel-some-instance-name" {
		t.Fatalf("embeddedContainerNameFor(non-embedded) = %q, want citadel-some-instance-name unconditionally", got)
	}
}

func TestEnsureEmbeddedComposeFile_NoOverrideWritesVerbatim(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	dir := t.TempDir()
	h := NewServiceHandler(dir)

	if err := h.ensureEmbeddedComposeFile("vllm"); err != nil {
		t.Fatalf("ensureEmbeddedComposeFile() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "services", "vllm.yml"))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(got) != embeddedservices.ServiceMap["vllm"] {
		t.Fatal("materialized compose file is not byte-identical to the embedded template without an override")
	}
}

// TestEnsureEmbeddedComposeFile_ExistingUnnamespacedFileIsReconciled mirrors
// the cmd-level fix: a compose file already materialized (no override, or by
// a pre-#860 binary) must be rewritten in place the next time
// ensureEmbeddedComposeFile runs under an active override, not left with the
// stale unnamespaced container_name forever.
func TestEnsureEmbeddedComposeFile_ExistingUnnamespacedFileIsReconciled(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("CITADEL_NODE_DIR", "")
	h := NewServiceHandler(dir)
	if err := h.ensureEmbeddedComposeFile("vllm"); err != nil {
		t.Fatalf("initial ensureEmbeddedComposeFile() error = %v", err)
	}

	overrideDir := t.TempDir()
	t.Setenv("CITADEL_NODE_DIR", overrideDir)
	if err := h.ensureEmbeddedComposeFile("vllm"); err != nil {
		t.Fatalf("ensureEmbeddedComposeFile() under override error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "services", "vllm.yml"))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	want := "container_name: " + compose.ContainerName("vllm", overrideDir)
	if !strings.Contains(string(got), want) {
		t.Fatalf("existing file was not reconciled: got %s, want it to contain %q", got, want)
	}
	if strings.Contains(string(got), "container_name: citadel-vllm\n") {
		t.Fatalf("existing file still carries the stale unnamespaced container_name: %s", got)
	}
}

func TestEnsureEmbeddedComposeFile_OverrideNamespacesContainerName(t *testing.T) {
	overrideDir := t.TempDir()
	t.Setenv("CITADEL_NODE_DIR", overrideDir)
	dir := t.TempDir()
	h := NewServiceHandler(dir)

	if err := h.ensureEmbeddedComposeFile("vllm"); err != nil {
		t.Fatalf("ensureEmbeddedComposeFile() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "services", "vllm.yml"))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}

	want := "container_name: " + compose.ContainerName("vllm", overrideDir)
	if !strings.Contains(string(got), want) {
		t.Fatalf("materialized compose file = %s, want it to contain %q", got, want)
	}
	if strings.Contains(string(got), "container_name: citadel-vllm\n") {
		t.Fatalf("materialized compose file still contains the unnamespaced line: %s", got)
	}
}
