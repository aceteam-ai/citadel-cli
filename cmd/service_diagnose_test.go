// cmd/service_diagnose_test.go
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveComposeContent_FromManifestOnDisk(t *testing.T) {
	dir := t.TempDir()
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	composeBody := "services:\n  vllm:\n    image: vllm/vllm-openai\n"
	composePath := filepath.Join(servicesDir, "vllm.yml")
	if err := os.WriteFile(composePath, []byte(composeBody), 0600); err != nil {
		t.Fatal(err)
	}

	manifest := &CitadelManifest{Services: []Service{{Name: "vllm", ComposeFile: "services/vllm.yml"}}}
	path, content, source := resolveComposeContent("vllm", manifest, dir)
	if source != "manifest" {
		t.Errorf("source = %q, want manifest", source)
	}
	if path != composePath {
		t.Errorf("path = %q, want %q", path, composePath)
	}
	if string(content) != composeBody {
		t.Errorf("content = %q, want %q", content, composeBody)
	}
}

func TestResolveComposeContent_ManifestEntryNotYetMaterialized_FallsBackToEmbedded(t *testing.T) {
	dir := t.TempDir()
	// Manifest declares "vllm" but the compose file was never written to disk.
	manifest := &CitadelManifest{Services: []Service{{Name: "vllm", ComposeFile: "services/vllm.yml"}}}
	path, content, source := resolveComposeContent("vllm", manifest, dir)
	if source != "embedded" {
		t.Errorf("source = %q, want embedded", source)
	}
	if path != "" {
		t.Errorf("path = %q, want empty (never materialized)", path)
	}
	if len(content) == 0 {
		t.Error("expected embedded catalog content for vllm")
	}
}

func TestResolveComposeContent_CatalogOnly(t *testing.T) {
	// Not in the manifest at all -- falls back straight to the embedded catalog.
	path, content, source := resolveComposeContent("bonsai", nil, "")
	if source != "embedded" || path != "" {
		t.Errorf("got path=%q source=%q, want path=\"\" source=embedded", path, source)
	}
	if len(content) == 0 {
		t.Error("expected embedded catalog content for bonsai")
	}
}

func TestResolveComposeContent_Unknown(t *testing.T) {
	path, content, source := resolveComposeContent("totally-unknown", nil, "")
	if path != "" || content != nil || source != "" {
		t.Errorf("got path=%q content=%v source=%q, want all empty", path, content, source)
	}
}

func TestResolveComposeContent_NeverWritesToDisk(t *testing.T) {
	dir := t.TempDir()
	manifest := &CitadelManifest{Services: []Service{{Name: "bonsai", ComposeFile: "services/bonsai.yml"}}}
	_, _, source := resolveComposeContent("bonsai", manifest, dir)
	if source != "embedded" {
		t.Fatalf("source = %q, want embedded", source)
	}
	if _, err := os.Stat(filepath.Join(dir, "services", "bonsai.yml")); !os.IsNotExist(err) {
		t.Error("resolveComposeContent must never materialize the compose file to disk")
	}
}

func TestResolveEnvForCompose_MergesSiblingEnvAndProcessEnv(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "vllm.yml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "vllm.env")
	if err := os.WriteFile(envPath, []byte("VLLM_MODEL=from-env-file\nSHARED=from-env-file\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SHARED", "from-process-env")
	env := resolveEnvForCompose(composePath)

	if env["VLLM_MODEL"] != "from-env-file" {
		t.Errorf("VLLM_MODEL = %q, want from-env-file (only source)", env["VLLM_MODEL"])
	}
	if env["SHARED"] != "from-process-env" {
		t.Errorf("SHARED = %q, want from-process-env (process env must win)", env["SHARED"])
	}
}

func TestResolveEnvForCompose_NoComposePath(t *testing.T) {
	// composePath == "" (never materialized): degrade to process env only,
	// never error/panic.
	t.Setenv("CITADEL_DIAGNOSE_TEST_VAR", "hello")
	env := resolveEnvForCompose("")
	if env["CITADEL_DIAGNOSE_TEST_VAR"] != "hello" {
		t.Errorf("expected process env to still be present, got %v", env["CITADEL_DIAGNOSE_TEST_VAR"])
	}
}

// TestBuildDiagnoseInput_PrefersComposeDeclaredContainerName pins the fix for
// a reported WANT: the container name was hardcoded "citadel-"+name,
// ignoring a compose file's actual container_name:, so a module-installed
// service that overrides the name (internal/catalog.ParseComposeContainerName
// exists for exactly this -- catalog install already uses it for conflict
// detection) got diagnosed against the wrong container and falsely reported
// "no container found".
func TestBuildDiagnoseInput_PrefersComposeDeclaredContainerName(t *testing.T) {
	dir := t.TempDir()
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	composeBody := "services:\n  acme:\n    image: acme/app\n    container_name: acme-app\n"
	composePath := filepath.Join(servicesDir, "acme.yml")
	if err := os.WriteFile(composePath, []byte(composeBody), 0600); err != nil {
		t.Fatal(err)
	}

	manifest := &CitadelManifest{Services: []Service{{Name: "acme", ComposeFile: "services/acme.yml"}}}
	in, err := buildDiagnoseInput("acme", manifest, dir, []string{"acme"})
	if err != nil {
		t.Fatalf("buildDiagnoseInput returned error: %v", err)
	}
	if in.ContainerName != "acme-app" {
		t.Errorf("ContainerName = %q, want the compose-declared %q", in.ContainerName, "acme-app")
	}
}

// TestBuildDiagnoseInput_MultiServiceComposeDoesNotOverrideContainerName
// guards against a narrow regression the container-name fix could otherwise
// introduce: catalog.ParseComposeContainerName returns the FIRST
// container_name: in the file with no notion of which `services:` key it
// belongs to. For a multi-service module compose (like the real
// services/nvr-service/compose.yml), trusting that value would silently
// diagnose an unrelated sibling container instead of the intended one.
func TestBuildDiagnoseInput_MultiServiceComposeDoesNotOverrideContainerName(t *testing.T) {
	dir := t.TempDir()
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	composeBody := "services:\n" +
		"  sidecar:\n    image: acme/sidecar\n    container_name: acme-sidecar\n" +
		"  main:\n    image: acme/main\n    container_name: acme-main\n"
	composePath := filepath.Join(servicesDir, "acme.yml")
	if err := os.WriteFile(composePath, []byte(composeBody), 0600); err != nil {
		t.Fatal(err)
	}

	manifest := &CitadelManifest{Services: []Service{{Name: "acme", ComposeFile: "services/acme.yml"}}}
	in, err := buildDiagnoseInput("acme", manifest, dir, []string{"acme"})
	if err != nil {
		t.Fatalf("buildDiagnoseInput returned error: %v", err)
	}
	if in.ContainerName != "citadel-acme" {
		t.Errorf("ContainerName = %q, want the conventional fallback %q for an ambiguous multi-service compose", in.ContainerName, "citadel-acme")
	}
}

// TestBuildDiagnoseInput_FallsBackToConventionalContainerName is the control:
// a compose file with no container_name: keeps the "citadel-<service>"
// convention.
func TestBuildDiagnoseInput_FallsBackToConventionalContainerName(t *testing.T) {
	dir := t.TempDir()
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	composeBody := "services:\n  vllm:\n    image: vllm/vllm-openai\n"
	composePath := filepath.Join(servicesDir, "vllm.yml")
	if err := os.WriteFile(composePath, []byte(composeBody), 0600); err != nil {
		t.Fatal(err)
	}

	manifest := &CitadelManifest{Services: []Service{{Name: "vllm", ComposeFile: "services/vllm.yml"}}}
	in, err := buildDiagnoseInput("vllm", manifest, dir, []string{"vllm"})
	if err != nil {
		t.Fatalf("buildDiagnoseInput returned error: %v", err)
	}
	if in.ContainerName != "citadel-vllm" {
		t.Errorf("ContainerName = %q, want the conventional %q", in.ContainerName, "citadel-vllm")
	}
}

// TestBuildDiagnoseInput_RefusesUnmaterializedComposeUnderNodeDirOverride
// pins the citadel#863 fix: a catalog-only service (the common first-use
// case -- never started, so nothing has ever been materialized into the
// override directory) must REFUSE rather than fall back to the embedded
// template's unnamespaced "citadel-<name>" container name, which -- on the
// shared-Docker-daemon topology --node-dir exists to be used safely against
// -- could be a DIFFERENT node's real, running container. This never touches
// docker: the assertion is purely on the returned error, mirroring how
// stopServiceByContainer's refuse-under-override case is tested.
func TestBuildDiagnoseInput_RefusesUnmaterializedComposeUnderNodeDirOverride(t *testing.T) {
	dir := t.TempDir()
	setNodeDirOverrideForTest(t, dir)

	_, err := buildDiagnoseInput("vllm", nil, dir, nil)
	if err == nil {
		t.Fatal("expected buildDiagnoseInput to refuse when no compose file is materialized under the --node-dir override")
	}
	if !strings.Contains(err.Error(), "--node-dir") {
		t.Errorf("error should mention --node-dir, got: %v", err)
	}
	if !strings.Contains(err.Error(), "citadel-vllm") {
		t.Errorf("error should name the REAL container it would have diagnosed (citadel-vllm), got: %v", err)
	}
}

// TestBuildDiagnoseInput_RefusesManifestEntryNotYetMaterializedUnderNodeDirOverride
// covers the sibling shape: the override's OWN manifest declares the
// service, but citadel.yaml alone doesn't materialize a compose file to
// disk -- resolveComposeContent still falls back to embedded content, so
// the same refusal must apply.
func TestBuildDiagnoseInput_RefusesManifestEntryNotYetMaterializedUnderNodeDirOverride(t *testing.T) {
	dir := t.TempDir()
	setNodeDirOverrideForTest(t, dir)

	manifest := &CitadelManifest{Services: []Service{{Name: "vllm", ComposeFile: "services/vllm.yml"}}}
	_, err := buildDiagnoseInput("vllm", manifest, dir, []string{"vllm"})
	if err == nil {
		t.Fatal("expected buildDiagnoseInput to refuse: citadel.yaml declares vllm but its compose file was never materialized under this override")
	}
}

// TestBuildDiagnoseInput_AllowsMaterializedComposeUnderNodeDirOverride is the
// control: once the override's OWN compose file is actually on disk inside
// the override directory (i.e. the service has been started under this
// override at least once), diagnose must proceed normally -- the container
// name it resolves is scoped to this override (citadel#860 namespacing, once
// materialization has run), so there is no real-container disclosure risk.
func TestBuildDiagnoseInput_AllowsMaterializedComposeUnderNodeDirOverride(t *testing.T) {
	dir := t.TempDir()
	setNodeDirOverrideForTest(t, dir)

	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	composeBody := "services:\n  vllm:\n    image: vllm/vllm-openai\n"
	if err := os.WriteFile(filepath.Join(servicesDir, "vllm.yml"), []byte(composeBody), 0600); err != nil {
		t.Fatal(err)
	}

	manifest := &CitadelManifest{Services: []Service{{Name: "vllm", ComposeFile: "services/vllm.yml"}}}
	in, err := buildDiagnoseInput("vllm", manifest, dir, []string{"vllm"})
	if err != nil {
		t.Fatalf("buildDiagnoseInput should not refuse once the override's own compose file is materialized: %v", err)
	}
	if in.ComposeSource != "manifest" {
		t.Errorf("ComposeSource = %q, want manifest", in.ComposeSource)
	}
}

// TestDiagnoseNodeDirRefusalError_NoOverrideIsANoOp pins that the no-override
// path is byte-identical to pre-#863 behavior regardless of source, matching
// every other --node-dir guard in this codebase.
func TestDiagnoseNodeDirRefusalError_NoOverrideIsANoOp(t *testing.T) {
	for _, source := range []string{"manifest", "embedded", ""} {
		if err := diagnoseNodeDirRefusalError("vllm", "", source, "citadel-vllm"); err != nil {
			t.Errorf("source=%q: expected nil error with no override active, got: %v", source, err)
		}
	}
}

func TestUnmanagedServiceGuidance_ListsManagedServices(t *testing.T) {
	msg := unmanagedServiceGuidance([]string{"my-custom-svc"})
	if !strings.Contains(msg, "my-custom-svc") {
		t.Errorf("guidance should list manifest services, got: %s", msg)
	}
	if !strings.Contains(msg, "vllm") {
		t.Errorf("guidance should list catalog services (e.g. vllm), got: %s", msg)
	}
	if !strings.Contains(msg, "citadel services") {
		t.Errorf("guidance should point at 'citadel services', got: %s", msg)
	}
}

func TestRunSvcDiagnose_InvalidServiceName(t *testing.T) {
	err := runSvcDiagnose(nil, []string{"; rm -rf /"})
	if err == nil {
		t.Fatal("expected an error for an invalid service name")
	}
}

func TestRunSvcDiagnose_UnmanagedServiceReturnsGuidance(t *testing.T) {
	// No citadel.yaml manifest is discoverable in this test environment, so
	// this exercises the fully-degraded "no manifest, not in catalog" path:
	// it must return a clear, actionable error rather than panicking.
	err := runSvcDiagnose(nil, []string{"definitely-not-a-real-service-name"})
	if err == nil {
		t.Fatal("expected an error for an unmanaged service name")
	}
	if !strings.Contains(err.Error(), "not a managed service") {
		t.Errorf("error = %v, want it to say the service isn't managed", err)
	}
}
