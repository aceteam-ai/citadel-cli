package compose

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

func TestNodeDirHash_EmptyReturnsEmpty(t *testing.T) {
	if got := NodeDirHash(""); got != "" {
		t.Fatalf("NodeDirHash(\"\") = %q, want empty", got)
	}
	if got := NodeDirHash("   "); got != "" {
		t.Fatalf("NodeDirHash(whitespace) = %q, want empty", got)
	}
}

func TestNodeDirHash_DeterministicForSameDir(t *testing.T) {
	first := NodeDirHash("/tmp/node-a")
	second := NodeDirHash("/tmp/node-a")
	if first == "" {
		t.Fatal("NodeDirHash returned empty for a non-empty dir")
	}
	if first != second {
		t.Fatalf("NodeDirHash must be deterministic: got %q then %q", first, second)
	}
	if len(first) != 12 {
		t.Fatalf("NodeDirHash length = %d, want 12", len(first))
	}
}

func TestNodeDirHash_DifferentDirsDifferentHash(t *testing.T) {
	a := NodeDirHash("/tmp/node-a")
	b := NodeDirHash("/tmp/node-b")
	if a == b {
		t.Fatalf("NodeDirHash(/tmp/node-a) == NodeDirHash(/tmp/node-b) = %q, want different hashes", a)
	}
}

func TestContainerName_NoOverrideIsUnchanged(t *testing.T) {
	if got := ContainerName("vllm", ""); got != "citadel-vllm" {
		t.Fatalf("ContainerName(vllm, \"\") = %q, want citadel-vllm (byte-identical to pre-#860)", got)
	}
}

func TestContainerName_WithOverrideIsNamespaced(t *testing.T) {
	dir := "/tmp/node-a"
	got := ContainerName("vllm", dir)
	want := "citadel-" + NodeDirHash(dir) + "-vllm"
	if got != want {
		t.Fatalf("ContainerName(vllm, %q) = %q, want %q", dir, got, want)
	}
	if !strings.HasPrefix(got, "citadel-") || !strings.HasSuffix(got, "-vllm") {
		t.Fatalf("ContainerName(vllm, %q) = %q, want citadel-<hash>-vllm shape", dir, got)
	}
}

func TestContainerName_TwoDifferentOverridesProduceDifferentNames(t *testing.T) {
	a := ContainerName("vllm", "/tmp/node-a")
	b := ContainerName("vllm", "/tmp/node-b")
	if a == b {
		t.Fatalf("ContainerName for two different override dirs must differ; both = %q", a)
	}
}

func TestRewriteContainerNameLine_Success(t *testing.T) {
	content := "services:\n  vllm:\n    image: vllm/vllm-openai\n    container_name: citadel-vllm\n"
	rewritten, err := RewriteContainerNameLine(content, "vllm", "citadel-abc123def456-vllm")
	if err != nil {
		t.Fatalf("RewriteContainerNameLine returned error: %v", err)
	}
	if !strings.Contains(rewritten, "container_name: citadel-abc123def456-vllm") {
		t.Fatalf("rewritten content missing new container_name line: %s", rewritten)
	}
	if strings.Contains(rewritten, "container_name: citadel-vllm\n") {
		t.Fatalf("rewritten content still contains the old container_name line: %s", rewritten)
	}
}

// TestRewriteContainerNameLine_DoesNotTouchImageTag pins the exact incident
// bonsai's compose would hit with a naive substring replace: it references
// "citadel-bonsai" TWICE (once as `image: citadel-bonsai:local`, once as
// `container_name: citadel-bonsai`), and only the second must change.
func TestRewriteContainerNameLine_DoesNotTouchImageTag(t *testing.T) {
	content := "services:\n  bonsai:\n    build: {context: ./bonsai}\n    image: citadel-bonsai:local\n    container_name: citadel-bonsai\n"
	rewritten, err := RewriteContainerNameLine(content, "bonsai", "citadel-abc123def456-bonsai")
	if err != nil {
		t.Fatalf("RewriteContainerNameLine returned error: %v", err)
	}
	if !strings.Contains(rewritten, "image: citadel-bonsai:local") {
		t.Fatalf("image tag must be untouched, got: %s", rewritten)
	}
	if !strings.Contains(rewritten, "container_name: citadel-abc123def456-bonsai") {
		t.Fatalf("container_name must be rewritten, got: %s", rewritten)
	}
	if strings.Count(rewritten, "citadel-abc123def456-bonsai") != 1 {
		t.Fatalf("expected exactly one namespaced occurrence, got: %s", rewritten)
	}
}

func TestRewriteContainerNameLine_MissingLineErrors(t *testing.T) {
	content := "services:\n  vllm:\n    image: vllm/vllm-openai\n"
	if _, err := RewriteContainerNameLine(content, "vllm", "citadel-abc-vllm"); err == nil {
		t.Fatal("expected an error when the expected container_name line is absent, got nil")
	}
}

// TestRewriteContainerNameLine_EveryServiceMapEntry is the table test the
// #860 review flagged as missing: RewriteContainerNameLine HARD-ERRORS when
// its expected "container_name: citadel-<svc>" line is absent, and that
// error now fails materialization under an active override. A future
// embedded service whose template quotes the value, uses different
// indentation, or pins a container_name that doesn't match its ServiceMap
// key would silently break override materialization at runtime with nothing
// catching it in CI -- this loops over the REAL embedded templates (not a
// synthetic string) to catch that at test time instead.
func TestRewriteContainerNameLine_EveryServiceMapEntry(t *testing.T) {
	for _, name := range services.GetAvailableServices() {
		content, ok := services.ServiceMap[name]
		if !ok {
			// GetAvailableServices can include manifest/module names beyond
			// ServiceMap in other contexts; here we only care about the
			// embedded templates this rewrite targets.
			continue
		}
		t.Run(name, func(t *testing.T) {
			before := strings.Count(content, "container_name: citadel-"+name)
			if before != 1 {
				t.Fatalf("services/compose/%s.yml must declare exactly ONE %q line, found %d",
					name, "container_name: citadel-"+name, before)
			}
			rewritten, err := RewriteContainerNameLine(content, name, "citadel-abc123def456-"+name)
			if err != nil {
				t.Fatalf("RewriteContainerNameLine(%s) returned error: %v", name, err)
			}
			if got := strings.Count(rewritten, "container_name: citadel-abc123def456-"+name); got != 1 {
				t.Fatalf("RewriteContainerNameLine(%s): want exactly 1 occurrence of the namespaced name, got %d", name, got)
			}
			if strings.Contains(rewritten, "container_name: citadel-"+name+"\n") {
				t.Fatalf("RewriteContainerNameLine(%s): old unnamespaced container_name line still present", name)
			}
		})
	}
}

func TestEnsureNamespacedContainerName_NoOverrideIsNoOp(t *testing.T) {
	content := "services:\n  vllm:\n    container_name: citadel-vllm\n"
	got, changed, err := EnsureNamespacedContainerName(content, "vllm", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatal("want no change with no override active")
	}
	if got != content {
		t.Fatalf("got %q, want the content unchanged", got)
	}
}

func TestEnsureNamespacedContainerName_RewritesUnnamespacedDefault(t *testing.T) {
	dir := "/tmp/node-a"
	content := "services:\n  vllm:\n    container_name: citadel-vllm\n"
	got, changed, err := EnsureNamespacedContainerName(content, "vllm", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("want changed=true: the existing file carried the unnamespaced default")
	}
	want := "container_name: " + ContainerName("vllm", dir)
	if !strings.Contains(got, want) {
		t.Fatalf("got %q, want it to contain %q", got, want)
	}
}

func TestEnsureNamespacedContainerName_AlreadyNamespacedIsNoOp(t *testing.T) {
	dir := "/tmp/node-a"
	already := "services:\n  vllm:\n    container_name: " + ContainerName("vllm", dir) + "\n"
	got, changed, err := EnsureNamespacedContainerName(already, "vllm", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatal("want no change: the file already carries the expected namespaced name")
	}
	if got != already {
		t.Fatalf("got %q, want unchanged content %q", got, already)
	}
}

// TestEnsureNamespacedContainerName_ForeignContentRefusesLoudly pins the
// review's third case: content matching NEITHER the expected namespaced name
// NOR the unnamespaced default (hand-edited by an operator, or materialized
// under a DIFFERENT override's hash) must be refused, not silently
// overwritten or silently left alone.
func TestEnsureNamespacedContainerName_ForeignContentRefusesLoudly(t *testing.T) {
	foreign := "services:\n  vllm:\n    container_name: citadel-nodedir-deadbeefcafe-vllm\n"
	if _, _, err := EnsureNamespacedContainerName(foreign, "vllm", "/tmp/node-a"); err == nil {
		t.Fatal("want a refusal error for content matching neither the expected nor the default container_name")
	}
}
