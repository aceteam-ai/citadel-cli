package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeFileTest(t *testing.T, path, body string) error {
	t.Helper()
	return os.WriteFile(path, []byte(body), 0600)
}

func fileExistsTest(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// parseOverride unmarshals a generated override into a generic map so tests can
// assert structure without depending on key order or formatting.
func parseOverride(t *testing.T, yml string) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(yml), &doc); err != nil {
		t.Fatalf("override is not valid YAML: %v\n---\n%s", err, yml)
	}
	return doc.Services
}

func strSlice(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a list, got %T (%v)", v, v)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("expected string list element, got %T", e)
		}
		out = append(out, s)
	}
	return out
}

func contains2(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestGenerateHardeningOverride_Defaults(t *testing.T) {
	base := `services:
  app:
    image: ghcr.io/x/y:latest
`
	out, err := GenerateHardeningOverride(base, &ServiceManifest{Name: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	svcs := parseOverride(t, out)
	app, ok := svcs["app"]
	if !ok {
		t.Fatalf("override missing service 'app'; got %v", svcs)
	}

	// security_opt: no-new-privileges
	if so := strSlice(t, app["security_opt"]); !contains2(so, "no-new-privileges:true") {
		t.Errorf("security_opt missing no-new-privileges: %v", so)
	}
	// cap_drop: ALL
	if cd := strSlice(t, app["cap_drop"]); !contains2(cd, "ALL") {
		t.Errorf("cap_drop should contain ALL: %v", cd)
	}
	// No cap_add by default (nothing declared, non-GPU).
	if _, present := app["cap_add"]; present {
		t.Errorf("cap_add should be absent when no caps declared: %v", app["cap_add"])
	}
	// read_only true
	if ro, _ := app["read_only"].(bool); !ro {
		t.Errorf("read_only should be true, got %v", app["read_only"])
	}
	// tmpfs contains /tmp
	if tm := strSlice(t, app["tmpfs"]); !contains2(tm, "/tmp") {
		t.Errorf("tmpfs should contain /tmp: %v", tm)
	}
	// resource defaults present
	if app["cpus"] != defaultSandboxCPUs {
		t.Errorf("cpus default = %v, want %v", app["cpus"], defaultSandboxCPUs)
	}
	if app["mem_limit"] != defaultSandboxMemory {
		t.Errorf("mem_limit default = %v, want %v", app["mem_limit"], defaultSandboxMemory)
	}
	if app["pids_limit"] != defaultSandboxPIDs {
		t.Errorf("pids_limit default = %v, want %v", app["pids_limit"], defaultSandboxPIDs)
	}
	// No host networking unless declared.
	if _, present := app["network_mode"]; present {
		t.Errorf("network_mode should be absent by default: %v", app["network_mode"])
	}
	// No devices unless declared.
	if _, present := app["devices"]; present {
		t.Errorf("devices should be absent by default: %v", app["devices"])
	}
}

func TestGenerateHardeningOverride_MultiService(t *testing.T) {
	base := `services:
  web:
    image: ghcr.io/x/web:latest
  db:
    image: postgres:16
`
	out, err := GenerateHardeningOverride(base, &ServiceManifest{Name: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	svcs := parseOverride(t, out)
	for _, name := range []string{"web", "db"} {
		svc, ok := svcs[name]
		if !ok {
			t.Fatalf("override missing service %q; got %v", name, svcs)
		}
		if cd := strSlice(t, svc["cap_drop"]); !contains2(cd, "ALL") {
			t.Errorf("service %q cap_drop should contain ALL: %v", name, cd)
		}
		if ro, _ := svc["read_only"].(bool); !ro {
			t.Errorf("service %q read_only should be true", name)
		}
	}
}

func TestGenerateHardeningOverride_DeclaredCapsAndWritablePaths(t *testing.T) {
	base := `services:
  app:
    image: ghcr.io/x/y:latest
`
	m := &ServiceManifest{
		Name: "x",
		Sandbox: SandboxSpec{
			Capabilities:  []string{"net_bind_service", "CAP_CHOWN", " "},
			WritablePaths: []string{"/var/cache", "/tmp", "/data"},
			Devices:       []string{"/dev/snd"},
			Resources:     SandboxResources{CPU: "4.0", Memory: "8g", PIDs: 1024},
		},
	}
	out, err := GenerateHardeningOverride(base, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	app := parseOverride(t, out)["app"]

	caps := strSlice(t, app["cap_add"])
	if !contains2(caps, "NET_BIND_SERVICE") {
		t.Errorf("cap_add should contain NET_BIND_SERVICE (uppercased): %v", caps)
	}
	if !contains2(caps, "CAP_CHOWN") {
		t.Errorf("cap_add should contain CAP_CHOWN verbatim: %v", caps)
	}
	if len(caps) != 2 {
		t.Errorf("blank cap should be dropped; got %v", caps)
	}

	tm := strSlice(t, app["tmpfs"])
	if !contains2(tm, "/var/cache") || !contains2(tm, "/data") {
		t.Errorf("tmpfs should include declared writable paths: %v", tm)
	}
	// /tmp present exactly once (declared duplicate de-duped).
	count := 0
	for _, p := range tm {
		if p == "/tmp" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("/tmp should appear exactly once, got %d: %v", count, tm)
	}

	if devs := strSlice(t, app["devices"]); !contains2(devs, "/dev/snd") {
		t.Errorf("devices should contain declared /dev/snd: %v", devs)
	}
	if app["cpus"] != "4.0" || app["mem_limit"] != "8g" || app["pids_limit"] != 1024 {
		t.Errorf("declared resources not honored: cpus=%v mem=%v pids=%v",
			app["cpus"], app["mem_limit"], app["pids_limit"])
	}
}

func TestGenerateHardeningOverride_HostNetworkOptIn(t *testing.T) {
	base := "services:\n  app:\n    image: x\n"

	// Default: no host networking.
	out, _ := GenerateHardeningOverride(base, &ServiceManifest{Name: "x"})
	if _, present := parseOverride(t, out)["app"]["network_mode"]; present {
		t.Errorf("network_mode must be absent unless host_network declared")
	}

	// Opt-in.
	m := &ServiceManifest{Name: "x", Sandbox: SandboxSpec{HostNetwork: true}}
	out, _ = GenerateHardeningOverride(base, m)
	if got := parseOverride(t, out)["app"]["network_mode"]; got != "host" {
		t.Errorf("network_mode = %v, want host", got)
	}
}

// TestGenerateHardeningOverride_GPUExempt encodes the TEI (#343) shape: a GPU
// service with a NVIDIA device reservation. Under #348 a GPU/inference service
// is EXEMPT from hardening -- the read-only rootfs, cap-drops, and (especially)
// the 2g/2cpu resource limits break inference. So the override must omit the GPU
// service entirely (no entry for it at all) and must never emit a GPU/deploy
// block.
func TestGenerateHardeningOverride_GPUExempt(t *testing.T) {
	base := `services:
  tei:
    image: ghcr.io/huggingface/text-embeddings-inference:latest
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: 1
              capabilities: [gpu]
`
	m := &ServiceManifest{
		Name:     "tei",
		Requires: Requirements{GPU: true},
		Sandbox: SandboxSpec{
			WritablePaths: []string{"/data"},
		},
	}
	out, err := GenerateHardeningOverride(base, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Override must not introduce any GPU/deploy reservation (base owns it).
	if strings.Contains(out, "deploy:") || strings.Contains(out, "reservations") || strings.Contains(out, "nvidia") {
		t.Errorf("override must not emit a GPU/deploy block:\n%s", out)
	}
	// The GPU service must be exempt: no override entry for it.
	if _, present := parseOverride(t, out)["tei"]; present {
		t.Errorf("GPU service 'tei' must be exempt (omitted from override):\n%s", out)
	}
}

// TestGenerateHardeningOverride_GPUSignals exercises each per-service GPU signal
// in isolation: each must exempt its service from the override.
func TestGenerateHardeningOverride_GPUSignals(t *testing.T) {
	cases := map[string]string{
		"gpus shorthand": `services:
  svc:
    image: x
    gpus: all
`,
		"runtime nvidia": `services:
  svc:
    image: x
    runtime: nvidia
`,
		"deploy reservation driver nvidia": `services:
  svc:
    image: x
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
`,
		"deploy reservation gpu capability": `services:
  svc:
    image: x
    deploy:
      resources:
        reservations:
          devices:
            - capabilities: [gpu]
`,
	}
	for name, base := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := GenerateHardeningOverride(base, &ServiceManifest{Name: "m"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, present := parseOverride(t, out)["svc"]; present {
				t.Errorf("GPU-signalled service must be exempt:\n%s", out)
			}
		})
	}
}

// TestGenerateHardeningOverride_MixedGPUAndSidecar is the discriminating case:
// an untrusted compose with a GPU inference service AND a non-GPU sidecar. The
// sidecar must be hardened; the GPU service must be untouched (exempt).
func TestGenerateHardeningOverride_MixedGPUAndSidecar(t *testing.T) {
	base := `services:
  inference:
    image: vllm/vllm-openai:latest
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
  proxy:
    image: nginx:latest
`
	out, err := GenerateHardeningOverride(base, &ServiceManifest{Name: "m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	svcs := parseOverride(t, out)
	if _, present := svcs["inference"]; present {
		t.Errorf("GPU service 'inference' must be exempt (omitted):\n%s", out)
	}
	proxy, ok := svcs["proxy"]
	if !ok {
		t.Fatalf("non-GPU sidecar 'proxy' must be hardened (present in override):\n%s", out)
	}
	if cd := strSlice(t, proxy["cap_drop"]); !contains2(cd, "ALL") {
		t.Errorf("sidecar should cap_drop ALL: %v", cd)
	}
	if ro, _ := proxy["read_only"].(bool); !ro {
		t.Errorf("sidecar should have read_only true")
	}
}

// TestGenerateHardeningOverride_PreservesBaseOverrides verifies the
// inject-only-where-absent rule: a hardening key the base service already
// declares is NOT clobbered by the override (so a module's explicit
// read_only:false / security_opt survives the `-f` merge).
func TestGenerateHardeningOverride_PreservesBaseOverrides(t *testing.T) {
	base := `services:
  app:
    image: x
    read_only: false
    mem_limit: 6g
    security_opt:
      - label=disable
`
	out, err := GenerateHardeningOverride(base, &ServiceManifest{Name: "m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	app := parseOverride(t, out)["app"]
	// Keys the base set must be absent from the override (not re-injected).
	if _, present := app["read_only"]; present {
		t.Errorf("read_only declared in base must not be injected by the override: %v", app["read_only"])
	}
	if _, present := app["mem_limit"]; present {
		t.Errorf("mem_limit declared in base must not be injected: %v", app["mem_limit"])
	}
	if _, present := app["security_opt"]; present {
		t.Errorf("security_opt declared in base must not be injected: %v", app["security_opt"])
	}
	// Keys the base did NOT set are still injected.
	if cd := strSlice(t, app["cap_drop"]); !contains2(cd, "ALL") {
		t.Errorf("cap_drop (not in base) should still be injected: %v", cd)
	}
}

func TestGenerateHardeningOverride_NoServicesError(t *testing.T) {
	if _, err := GenerateHardeningOverride("name: not-a-compose\n", &ServiceManifest{Name: "x"}); err == nil {
		t.Error("expected an error for a compose with no services")
	}
	if _, err := GenerateHardeningOverride(":::not yaml:::", &ServiceManifest{Name: "x"}); err == nil {
		t.Error("expected an error for invalid YAML")
	}
}

func TestGenerateHardeningOverride_Deterministic(t *testing.T) {
	base := "services:\n  b:\n    image: x\n  a:\n    image: y\n"
	a, _ := GenerateHardeningOverride(base, &ServiceManifest{Name: "x"})
	b, _ := GenerateHardeningOverride(base, &ServiceManifest{Name: "x"})
	if a != b {
		t.Errorf("override generation should be deterministic:\n%s\n---\n%s", a, b)
	}
}

// --- bind-mount confinement ---

func TestBindMountViolations(t *testing.T) {
	servicesDir := "/home/u/citadel-node/services"
	name := "mod"
	allowed := SandboxDataDir(servicesDir, name) // /home/u/citadel-node/services/mod-data

	tests := []struct {
		desc       string
		compose    string
		wantViols  []string
		wantNoViol bool
	}{
		{
			desc: "named volume is fine",
			compose: `services:
  app:
    image: x
    volumes:
      - mydata:/var/lib/data
`,
			wantNoViol: true,
		},
		{
			desc: "bind mount inside sandbox data dir is fine",
			compose: `services:
  app:
    image: x
    volumes:
      - ` + filepath.Join(allowed, "sub") + `:/data
`,
			wantNoViol: true,
		},
		{
			desc: "relative bind resolving into sandbox data dir is fine",
			compose: `services:
  app:
    image: x
    volumes:
      - ./mod-data/cache:/cache
`,
			wantNoViol: true,
		},
		{
			desc: "absolute bind outside is a violation",
			compose: `services:
  app:
    image: x
    volumes:
      - /etc:/host-etc
`,
			wantViols: []string{"/etc"},
		},
		{
			desc: "home bind is a violation",
			compose: `services:
  app:
    image: x
    volumes:
      - ~/secrets:/secrets
`,
			wantViols: []string{"~/secrets"},
		},
		{
			desc: "sibling dir with shared prefix is NOT within sandbox dir",
			compose: `services:
  app:
    image: x
    volumes:
      - ` + servicesDir + `/mod-data-evil:/x
`,
			wantViols: []string{servicesDir + "/mod-data-evil"},
		},
		{
			desc: "multiple binds, mixed",
			compose: `services:
  app:
    image: x
    volumes:
      - ` + filepath.Join(allowed, "ok") + `:/ok
      - /var/run/foo:/bad
`,
			wantViols: []string{"/var/run/foo"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := BindMountViolations(tc.compose, servicesDir, name)
			if tc.wantNoViol {
				if len(got) != 0 {
					t.Errorf("expected no violations, got %v", got)
				}
				return
			}
			if len(got) != len(tc.wantViols) {
				t.Fatalf("violations = %v, want %v", got, tc.wantViols)
			}
			for i, w := range tc.wantViols {
				if got[i] != w {
					t.Errorf("violation[%d] = %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

// --- InstallFromManifest sandbox wiring ---

func TestInstallFromManifest_UntrustedWritesSandboxOverride(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yml")
	if err := writeFileTest(t, composePath, "services:\n  app:\n    image: ghcr.io/x/y:latest\n"); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	manifest := &ServiceManifest{Name: "mod"}

	// untrusted=true must generate the override and flag the result.
	res, err := InstallFromManifest(manifest, composePath, servicesDir, nil, false, false, true, false)
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if !res.Sandboxed {
		t.Fatal("result.Sandboxed should be true for an untrusted install")
	}
	want := filepath.Join(servicesDir, "mod.sandbox.yml")
	if res.SandboxOverridePath != want {
		t.Errorf("override path = %q, want %q", res.SandboxOverridePath, want)
	}
	if !fileExistsTest(want) {
		t.Errorf("override file %q was not written", want)
	}
	// The per-module sandbox data dir must be created.
	if !fileExistsTest(SandboxDataDir(servicesDir, "mod")) {
		t.Errorf("sandbox data dir %q was not created", SandboxDataDir(servicesDir, "mod"))
	}
}

func TestInstallFromManifest_TrustedNoSandboxOverride(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yml")
	if err := writeFileTest(t, composePath, "services:\n  app:\n    image: ghcr.io/x/y:latest\n"); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	manifest := &ServiceManifest{Name: "mod"}

	// untrusted=false (Tier 0/1): no override, not flagged.
	res, err := InstallFromManifest(manifest, composePath, servicesDir, nil, false, true, false, false)
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if res.Sandboxed {
		t.Error("trusted install must not be sandboxed")
	}
	if fileExistsTest(filepath.Join(servicesDir, "mod.sandbox.yml")) {
		t.Error("trusted install must not write a sandbox override")
	}
}

func TestInstallFromManifest_BindMountConfinement(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yml")
	// A bind-mount of /etc (outside the sandbox data dir).
	body := "services:\n  app:\n    image: x\n    volumes:\n      - /etc:/host-etc\n"
	if err := writeFileTest(t, composePath, body); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	manifest := &ServiceManifest{Name: "mod"}

	// untrusted + !allowPrivileged: must REFUSE.
	if _, err := InstallFromManifest(manifest, composePath, servicesDir, nil, false, false, true, false); err == nil {
		t.Fatal("expected refusal for untrusted module bind-mounting outside sandbox dir")
	}

	// allowPrivileged overrides the confinement.
	if _, err := InstallFromManifest(manifest, composePath, servicesDir, nil, false, true, true, false); err != nil {
		t.Fatalf("allow-privileged should bypass bind-mount confinement, got %v", err)
	}

	// A trusted install (untrusted=false) is not confined either.
	dir2 := t.TempDir()
	composePath2 := filepath.Join(dir2, "compose.yml")
	if err := writeFileTest(t, composePath2, body); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallFromManifest(manifest, composePath2, filepath.Join(dir2, "services"), nil, false, true, false, false); err != nil {
		t.Fatalf("trusted install should not be bind-confined, got %v", err)
	}
}

func TestWithinDir(t *testing.T) {
	cases := []struct {
		dir, path string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a/b-data", false},
		{"/a/b", "/a", false},
	}
	for _, c := range cases {
		if got := withinDir(c.dir, c.path); got != c.want {
			t.Errorf("withinDir(%q,%q) = %v, want %v", c.dir, c.path, got, c.want)
		}
	}
}
