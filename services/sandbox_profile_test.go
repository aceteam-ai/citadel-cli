package services

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parsedSandboxDoc captures the fields the profile invariants assert.
type parsedSandboxDoc struct {
	Services map[string]parsedSandboxSvc `yaml:"services"`
	Networks map[string]parsedSandboxNet `yaml:"networks"`
}

type parsedSandboxSvc struct {
	Image         string                         `yaml:"image"`
	ContainerName string                         `yaml:"container_name"`
	ReadOnly      bool                           `yaml:"read_only"`
	CapDrop       []string                       `yaml:"cap_drop"`
	SecurityOpt   []string                       `yaml:"security_opt"`
	PidsLimit     int                            `yaml:"pids_limit"`
	MemLimit      string                         `yaml:"mem_limit"`
	CPUs          string                         `yaml:"cpus"`
	Ports         []string                       `yaml:"ports"`
	NetworkMode   string                         `yaml:"network_mode"`
	Volumes       []any                          `yaml:"volumes"`
	Networks      map[string]parsedSandboxAttach `yaml:"networks"`
	Environment   []string                       `yaml:"environment"`
}

type parsedSandboxAttach struct {
	IPv4Address string `yaml:"ipv4_address"`
}

type parsedSandboxNet struct {
	Driver   string `yaml:"driver"`
	Internal bool   `yaml:"internal"`
	IPAM     struct {
		Config []struct {
			Subnet  string `yaml:"subnet"`
			Gateway string `yaml:"gateway"`
		} `yaml:"config"`
	} `yaml:"ipam"`
}

func mustParseSandbox(t *testing.T, yamlStr string) parsedSandboxDoc {
	t.Helper()
	var doc parsedSandboxDoc
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		t.Fatalf("rendered compose is not valid YAML: %v\n%s", err, yamlStr)
	}
	return doc
}

// assertSandboxContract asserts the full enforced runtime contract on one
// rendered module. It is the single place every control is checked so both
// demonstrated modules go through the identical assertions.
func assertSandboxContract(t *testing.T, spec SandboxedModuleSpec, yamlStr string) {
	t.Helper()
	doc := mustParseSandbox(t, yamlStr)

	// Exactly one service, one network (the advisor's "one service, one network"
	// invariant: same-internal-network reachability is empty by construction).
	if len(doc.Services) != 1 {
		t.Fatalf("expected exactly one service, got %d: %v", len(doc.Services), doc.Services)
	}
	if len(doc.Networks) != 1 {
		t.Fatalf("expected exactly one network, got %d: %v", len(doc.Networks), doc.Networks)
	}

	svc, ok := doc.Services[spec.Name]
	if !ok {
		t.Fatalf("service %q not found in rendered compose", spec.Name)
	}

	// Image: present and registry-qualified (the TestCheckedInComposeImagesAreRegistryQualified
	// invariant, asserted on the rendered output per spec).
	if svc.Image != spec.Image {
		t.Errorf("image = %q, want %q", svc.Image, spec.Image)
	}
	if !registryQualifiedImage(svc.Image) {
		t.Errorf("rendered image %q is not registry-qualified", svc.Image)
	}
	if !IsRegistryQualifiedImage(svc.Image) {
		t.Errorf("IsRegistryQualifiedImage(%q) = false", svc.Image)
	}

	// Container name.
	if want := SandboxedModuleContainerName(spec.Name); svc.ContainerName != want {
		t.Errorf("container_name = %q, want %q", svc.ContainerName, want)
	}

	// Hardening posture.
	if !svc.ReadOnly {
		t.Error("read_only must be true (read-only root filesystem)")
	}
	if len(svc.CapDrop) != 1 || svc.CapDrop[0] != SandboxedModuleCapDrop {
		t.Errorf("cap_drop = %v, want [%q]", svc.CapDrop, SandboxedModuleCapDrop)
	}
	if len(svc.SecurityOpt) != 1 || svc.SecurityOpt[0] != SandboxedModuleNoNewPrivileges {
		t.Errorf("security_opt = %v, want [%q]", svc.SecurityOpt, SandboxedModuleNoNewPrivileges)
	}
	if svc.PidsLimit <= 0 {
		t.Errorf("pids_limit = %d, want > 0", svc.PidsLimit)
	}
	if strings.TrimSpace(svc.MemLimit) == "" {
		t.Error("mem_limit must be set")
	}
	if strings.TrimSpace(svc.CPUs) == "" {
		t.Error("cpus must be set")
	}

	// NO published ports: docker drops publishes on an internal network, so a
	// published port here would be a latent bug. The host reaches the module at
	// its static IP instead.
	if len(svc.Ports) != 0 {
		t.Errorf("sandboxed module must publish NO ports (reached at static IP); got %v", svc.Ports)
	}
	// NO network_mode override (we use a dedicated internal network, not host/none).
	if svc.NetworkMode != "" {
		t.Errorf("network_mode must be unset; got %q", svc.NetworkMode)
	}

	// Loopback/no-publish invariant via the single bind authority: an internal-
	// network profile publishes nothing, so hasPublish must be false. We do NOT
	// lean on TestServiceMapBindSweep passing vacuously (it would say nothing);
	// the real off-host-unreachability guarantee is the no-ports + internal-net
	// assertions above.
	if _, hasPublish := ComposePublishesLoopbackOnly(yamlStr, nil); hasPublish {
		t.Error("sandboxed module must not declare any host port publish")
	}

	// NO host bind mounts: every volume entry must be a long-form tmpfs mount
	// (a map with type: tmpfs), never a "host:container" string bind.
	foundTmp := false
	for _, v := range svc.Volumes {
		m, isMap := v.(map[string]any)
		if !isMap {
			t.Errorf("volume entry %v must be a long-form tmpfs mount, not a host bind/short form", v)
			continue
		}
		if m["type"] != "tmpfs" {
			t.Errorf("volume %v must be type tmpfs (no host bind mounts allowed)", m)
		}
		if _, hasSource := m["source"]; hasSource {
			t.Errorf("volume %v must have no host source (no bind mounts)", m)
		}
		if m["target"] == "/tmp" {
			foundTmp = true
		}
	}
	if !foundTmp {
		t.Error("tmpfs scratch /tmp must be present")
	}

	// Network: dedicated, internal, with the deterministic subnet/gateway/IP.
	netName := SandboxedModuleNetworkName(spec.Name)
	net, ok := doc.Networks[netName]
	if !ok {
		t.Fatalf("network %q not found; networks=%v", netName, doc.Networks)
	}
	if !net.Internal {
		t.Error("network must be internal: true (NO outbound network -- the hard security property)")
	}
	if len(net.IPAM.Config) != 1 {
		t.Fatalf("network ipam config = %v, want exactly one", net.IPAM.Config)
	}
	if got, want := net.IPAM.Config[0].Subnet, SandboxedModuleSubnet(spec.NetworkIndex); got != want {
		t.Errorf("subnet = %q, want %q", got, want)
	}
	if got, want := net.IPAM.Config[0].Gateway, SandboxedModuleGateway(spec.NetworkIndex); got != want {
		t.Errorf("gateway = %q, want %q", got, want)
	}

	// Service attached to exactly that one network at the static IP.
	if len(svc.Networks) != 1 {
		t.Fatalf("service must attach exactly one network; got %v", svc.Networks)
	}
	attach, ok := svc.Networks[netName]
	if !ok {
		t.Fatalf("service not attached to %q; got %v", netName, svc.Networks)
	}
	if got, want := attach.IPv4Address, SandboxedModuleIP(spec.NetworkIndex); got != want {
		t.Errorf("static ipv4_address = %q, want %q", got, want)
	}

	// The static IP lies inside the subnet (belt and suspenders).
	if !sandboxedModuleIPInSubnet(spec.NetworkIndex) {
		t.Errorf("static IP %s not within subnet %s", SandboxedModuleIP(spec.NetworkIndex), SandboxedModuleSubnet(spec.NetworkIndex))
	}

	// Endpoint helper agrees with the rendered IP:port.
	if want := SandboxedModuleIP(spec.NetworkIndex) + ":" + itoa(spec.ContainerPort); SandboxedModuleEndpoint(spec) != want {
		t.Errorf("SandboxedModuleEndpoint = %q, want %q", SandboxedModuleEndpoint(spec), want)
	}
}

func itoa(n int) string {
	// tiny local helper to avoid importing strconv for one use
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestSandboxedModuleProfileEnforcesContract is the acceptance proof that TWO
// different helper modules run on the SAME profile with no per-module runtime
// changes: both go through RenderSandboxedModuleCompose and get an identical
// enforced contract (no egress, read-only rootfs, caps dropped, no bind mounts,
// host-private endpoint). The first spec mirrors #1264's extractor; the second
// is an unrelated media-probe helper.
func TestSandboxedModuleProfileEnforcesContract(t *testing.T) {
	extractor := SandboxedModuleSpec{
		Name:          "extract",
		Image:         "ghcr.io/aceteam-ai/extract-service:latest",
		ContainerPort: 8100,
		NetworkIndex:  0,
		Scratch:       []SandboxScratch{{Path: "/work"}},
	}
	mediaProbe := SandboxedModuleSpec{
		Name:          "media-probe",
		Image:         "docker.io/library/mediainfo:latest",
		ContainerPort: 9000,
		NetworkIndex:  7,
		MemoryLimit:   "1g",
		CPUs:          "1.0",
		PIDsLimit:     128,
		Env:           map[string]string{"PROBE_MODE": "safe"},
	}

	for _, spec := range []SandboxedModuleSpec{extractor, mediaProbe} {
		t.Run(spec.Name, func(t *testing.T) {
			yamlStr, err := RenderSandboxedModuleCompose(spec)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			assertSandboxContract(t, spec, yamlStr)
		})
	}

	// Prove the two modules share the IDENTICAL hardening posture (one contract,
	// not redefined per module): the control fields match byte-for-byte.
	y1, err := RenderSandboxedModuleCompose(extractor)
	if err != nil {
		t.Fatal(err)
	}
	y2, err := RenderSandboxedModuleCompose(mediaProbe)
	if err != nil {
		t.Fatal(err)
	}
	s1 := mustParseSandbox(t, y1).Services[extractor.Name]
	s2 := mustParseSandbox(t, y2).Services[mediaProbe.Name]
	if !eqStrings(s1.CapDrop, s2.CapDrop) {
		t.Errorf("cap_drop differs between modules: %v vs %v", s1.CapDrop, s2.CapDrop)
	}
	if !eqStrings(s1.SecurityOpt, s2.SecurityOpt) {
		t.Errorf("security_opt differs between modules: %v vs %v", s1.SecurityOpt, s2.SecurityOpt)
	}
	if s1.ReadOnly != s2.ReadOnly {
		t.Errorf("read_only differs: %v vs %v", s1.ReadOnly, s2.ReadOnly)
	}
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSandboxedModuleRenderDeterministic: identical spec -> byte-identical output.
func TestSandboxedModuleRenderDeterministic(t *testing.T) {
	spec := SandboxedModuleSpec{
		Name:          "extract",
		Image:         "ghcr.io/aceteam-ai/extract-service:latest",
		ContainerPort: 8100,
		NetworkIndex:  3,
		Env:           map[string]string{"B": "2", "A": "1", "C": "3"},
	}
	a, err := RenderSandboxedModuleCompose(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderSandboxedModuleCompose(spec)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("render not deterministic:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	// Env rendered sorted.
	if !strings.Contains(a, "- A=1") || !strings.Contains(a, "- B=2") || !strings.Contains(a, "- C=3") {
		t.Errorf("env not rendered as sorted K=V list:\n%s", a)
	}
	if strings.Index(a, "A=1") > strings.Index(a, "B=2") {
		t.Errorf("env not sorted:\n%s", a)
	}
}

func TestSandboxedModuleSpecValidate(t *testing.T) {
	base := SandboxedModuleSpec{Name: "ok", Image: "docker.io/library/x:1", ContainerPort: 8080, NetworkIndex: 0}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(s *SandboxedModuleSpec)
	}{
		{"empty name", func(s *SandboxedModuleSpec) { s.Name = "" }},
		{"bad name chars", func(s *SandboxedModuleSpec) { s.Name = "Bad_Name" }},
		{"leading dash", func(s *SandboxedModuleSpec) { s.Name = "-x" }},
		{"empty image", func(s *SandboxedModuleSpec) { s.Image = "" }},
		{"short image", func(s *SandboxedModuleSpec) { s.Image = "alpine:latest" }},
		{"bad port", func(s *SandboxedModuleSpec) { s.ContainerPort = 0 }},
		{"index too high", func(s *SandboxedModuleSpec) { s.NetworkIndex = SandboxedModuleMaxIndex + 1 }},
		{"index negative", func(s *SandboxedModuleSpec) { s.NetworkIndex = -1 }},
		{"relative scratch", func(s *SandboxedModuleSpec) { s.Scratch = []SandboxScratch{{Path: "rel/dir"}} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := base
			c.mut(&s)
			if err := s.Validate(); err == nil {
				t.Errorf("expected validation error for %s", c.name)
			}
			if _, err := RenderSandboxedModuleCompose(s); err == nil {
				t.Errorf("RenderSandboxedModuleCompose should refuse %s", c.name)
			}
		})
	}
}

// TestSandboxedModuleAddressesDistinctPerIndex: distinct indices -> distinct,
// non-overlapping subnets and distinct static IPs (reviewable indexed allocation,
// no hashing). A reachability hole from a collision is thus structurally
// impossible.
func TestSandboxedModuleAddressesDistinctPerIndex(t *testing.T) {
	seenIP := map[string]int{}
	seenSubnet := map[string]int{}
	for i := 0; i <= SandboxedModuleMaxIndex; i++ {
		if !sandboxedModuleIPInSubnet(i) {
			t.Fatalf("index %d: static IP %s not in subnet %s", i, SandboxedModuleIP(i), SandboxedModuleSubnet(i))
		}
		ip := SandboxedModuleIP(i)
		if prev, dup := seenIP[ip]; dup {
			t.Fatalf("static IP %s reused by indices %d and %d", ip, prev, i)
		}
		seenIP[ip] = i
		sn := SandboxedModuleSubnet(i)
		if prev, dup := seenSubnet[sn]; dup {
			t.Fatalf("subnet %s reused by indices %d and %d", sn, prev, i)
		}
		seenSubnet[sn] = i
	}
}
