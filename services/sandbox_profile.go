// services/sandbox_profile.go
//
// Generic sandboxed-module runtime profile (aceteam-ai/citadel-cli#1288, a slice
// of aceteam#10876). ONE reusable least-privilege runtime contract for on-node
// helper services that parse UNTRUSTED input (extraction, OCR, media probing,
// archive unpacking, future converters) -- so each such module does NOT redefine
// its own containment posture.
//
// This file is the SINGLE AUTHORITY for that contract. RenderSandboxedModuleCompose
// takes a SandboxedModuleSpec (name + registry-qualified image + the port the
// module listens on) and emits a docker/podman compose file that enforces, for
// every module identically:
//
//   - No outbound network. The service attaches to exactly ONE user-defined
//     bridge network declared `internal: true`. An internal network has no
//     NAT/default route, so the container cannot reach the internet or the mesh
//     -- VERIFIED empirically (a container on such a network has no default route
//     and every egress connect fails). This is the "equivalent the runtime seam
//     supports" that the issue's `network_mode: none` suggestion stands in for:
//     `network_mode: none` would ALSO drop the loopback endpoint below (docker
//     silently discards published/served ports with no network stack), whereas
//     `internal: true` keeps the host able to reach the module while still
//     blocking egress.
//   - Host-private, loopback-equivalent endpoint. The module is reached by the
//     co-located citadel process at the service's STATIC bridge IP
//     (SandboxedModuleIP) on the internal network -- a subnet that is routable
//     ONLY on this host (never advertised to the LAN, never reachable over the
//     mesh). We deliberately do NOT publish a `127.0.0.1:host:cport` port:
//     docker silently drops published ports on an internal network, so a
//     published port here would be a latent bug. The static IP is derived from
//     the spec's NetworkIndex so the endpoint is known BEFORE the container is up
//     and is stable across restarts.
//   - No host bind mounts. The rendered compose declares NO host-path volumes;
//     the module never sees the node filesystem. The only mounts are tmpfs
//     scratch (no host source).
//   - Read-only root filesystem (`read_only: true`) + tmpfs scratch (/tmp plus
//     any declared SandboxScratch), size-capped so an untrusted parser cannot
//     exhaust host RAM.
//   - `cap_drop: ALL`, `security_opt: no-new-privileges:true`.
//   - `pids_limit`, `mem_limit`, `cpus` -- resource ceilings (conservative
//     defaults when the spec leaves them unset).
//
// The input/output model: input arrives as bytes over that host-private endpoint
// (the module's own HTTP/socket surface), output streams back; the module never
// touches the node filesystem or the mesh. A module's image must be
// SELF-CONTAINED -- no network at runtime means no model/weight download at
// startup and no `~/citadel-cache` mount; anything the module needs must be baked
// into its image. This is a contract term on consumers (e.g. #1264's extractor
// image).
//
// Residual, documented (not a defect this profile can close): a container on an
// internal bridge can still reach the bridge's own gateway IP, i.e. host services
// bound to 0.0.0.0 (ollama's default host publish). Compose cannot block that;
// callers who care must bind such host services loopback-only (services/bind.go).
//
// services is a leaf package (pure, yaml only); the runtime-seam probe, the typed
// dispatch-refusal error, and the #1287 lifecycle preset live in internal/jobs
// (which may import catalog), and the heartbeat capability lives in
// internal/status.
package services

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Enforced-control constants. Pinned by TestSandboxedModuleProfileEnforcesContract
// so a future edit that weakens the posture fails loudly rather than silently.
const (
	// SandboxedModuleCapDrop is the single cap_drop entry: drop every Linux
	// capability. A module that genuinely needs a capability is NOT a candidate
	// for this profile.
	SandboxedModuleCapDrop = "ALL"
	// SandboxedModuleNoNewPrivileges is the security_opt that forbids a child
	// process gaining privileges via setuid binaries.
	SandboxedModuleNoNewPrivileges = "no-new-privileges:true"

	// Conservative resource defaults, applied when the spec leaves a field unset.
	defaultSandboxedModuleMemory    = "2g"
	defaultSandboxedModuleCPUs      = "2.0"
	defaultSandboxedModulePIDs      = 512
	defaultSandboxedModuleTmpBytes  = int64(64 << 20) // 64 MiB tmpfs /tmp cap.
	sandboxedModuleScratchTmpfsSize = int64(64 << 20) // default cap for extra scratch mounts.

	// sandboxedModuleNetworkOctets is the private /24-per-index base for the
	// per-module internal subnets. Each module gets a dedicated /29 at
	// <base>.<NetworkIndex>.0/29 (8 addresses: .0 network, .1 gateway, .2 the
	// service, .7 broadcast). 10.200.0.0/16 is deliberately OUTSIDE docker's
	// default address pool (172.16/12) and podman's default (10.88/16), so these
	// fixed subnets do not collide with dynamically-created engine networks. A
	// node whose LAN already uses 10.200.x is the documented edge: `network
	// create` fails loudly (pool overlap) rather than silently misrouting.
	sandboxedModuleNetworkOctets = "10.200"
	// SandboxedModuleMaxIndex is the inclusive upper bound on NetworkIndex (the
	// third octet). 256 distinct sandboxed modules is far beyond any realistic
	// node.
	SandboxedModuleMaxIndex = 255

	// sandboxedModuleContainerPrefix / sandboxedModuleNetworkSuffix name the
	// container and network deterministically from the module name.
	sandboxedModuleContainerPrefix = "citadel-sandbox-"
	sandboxedModuleNetworkSuffix   = "-sandbox"
)

// SandboxScratch is one writable tmpfs mount inside the sandbox beyond /tmp (some
// parsers need a work dir). SizeBytes caps it; <=0 uses a conservative default.
type SandboxScratch struct {
	Path      string
	SizeBytes int64
}

// SandboxedModuleSpec describes ONE on-node helper module to run under the
// sandboxed profile. The runtime contract (no egress, read-only rootfs, caps
// dropped, loopback-equivalent endpoint) is NOT in the spec -- it is enforced
// identically by RenderSandboxedModuleCompose for every spec. The spec carries
// only the per-module identity and resource knobs.
type SandboxedModuleSpec struct {
	// Name is the module id. The container is citadel-sandbox-<Name> and its
	// dedicated internal network is <Name>-sandbox. Lowercase alnum + dashes.
	Name string
	// Image is the module's container image. MUST be registry-qualified (so it
	// resolves on rootless Podman, same rule as the embedded ServiceMap) and
	// self-contained (no runtime network to pull anything).
	Image string
	// ContainerPort is the TCP port the module listens on inside the container.
	// citadel reaches it at SandboxedModuleIP(NetworkIndex):ContainerPort.
	ContainerPort int
	// NetworkIndex (0..SandboxedModuleMaxIndex) selects the deterministic /29
	// internal subnet and the service's static IP. Distinct modules MUST use
	// distinct indices; the caller/registry owns allocation.
	NetworkIndex int
	// MemoryLimit / CPUs / PIDsLimit are the compose resource ceilings. Zero
	// values fall back to the conservative defaults above.
	MemoryLimit string
	CPUs        string
	PIDsLimit   int
	// TmpSizeBytes caps the /tmp tmpfs (<=0 uses the 64 MiB default).
	TmpSizeBytes int64
	// Scratch declares writable tmpfs mounts beyond /tmp.
	Scratch []SandboxScratch
	// Env is optional environment for the module (rendered as a sorted K=V list).
	Env map[string]string
	// Command optionally overrides the image's entrypoint command (compose
	// `command:`). Omitted when nil, so the image's own CMD runs.
	Command []string
}

// SandboxedModuleContainerName returns the deterministic container name for a
// module (citadel-sandbox-<name>).
func SandboxedModuleContainerName(name string) string {
	return sandboxedModuleContainerPrefix + name
}

// SandboxedModuleNetworkName returns the deterministic dedicated internal network
// name for a module (<name>-sandbox).
func SandboxedModuleNetworkName(name string) string {
	return name + sandboxedModuleNetworkSuffix
}

// SandboxedModuleSubnet returns the /29 internal subnet for a module index.
func SandboxedModuleSubnet(index int) string {
	return fmt.Sprintf("%s.%d.0/29", sandboxedModuleNetworkOctets, index)
}

// SandboxedModuleGateway returns the gateway IP (.1) of a module index's subnet.
func SandboxedModuleGateway(index int) string {
	return fmt.Sprintf("%s.%d.1", sandboxedModuleNetworkOctets, index)
}

// SandboxedModuleIP returns the STATIC service IP (.2) for a module index. This
// is the host-private, loopback-equivalent endpoint address citadel dials; it is
// known before the container starts and stable across restarts.
func SandboxedModuleIP(index int) string {
	return fmt.Sprintf("%s.%d.2", sandboxedModuleNetworkOctets, index)
}

// SandboxedModuleEndpoint returns the host-private "ip:port" the co-located
// citadel process uses to reach the module.
func SandboxedModuleEndpoint(spec SandboxedModuleSpec) string {
	return fmt.Sprintf("%s:%d", SandboxedModuleIP(spec.NetworkIndex), spec.ContainerPort)
}

// IsRegistryQualifiedImage reports whether an image reference names its registry
// explicitly (so it resolves on rootless Podman, which refuses short names). It
// is the production counterpart of the TestCheckedInComposeImagesAreRegistryQualified
// invariant and is used by SandboxedModuleSpec.Validate.
func IsRegistryQualifiedImage(image string) bool {
	first, _, ok := strings.Cut(strings.TrimSpace(image), "/")
	if !ok {
		return false
	}
	return first == "localhost" || strings.Contains(first, ".") || strings.Contains(first, ":")
}

// Validate checks the spec's identity, image, port and index. A bad spec must
// fail here rather than render a broken or insecure compose.
func (s SandboxedModuleSpec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("sandboxed module: Name is required")
	}
	if !validSandboxedModuleName(s.Name) {
		return fmt.Errorf("sandboxed module %q: Name must be lowercase alphanumeric with dashes", s.Name)
	}
	if strings.TrimSpace(s.Image) == "" {
		return fmt.Errorf("sandboxed module %q: Image is required", s.Name)
	}
	if !IsRegistryQualifiedImage(s.Image) {
		return fmt.Errorf("sandboxed module %q: Image %q is not registry-qualified (name the registry explicitly, e.g. docker.io/... or ghcr.io/...)", s.Name, s.Image)
	}
	if s.ContainerPort <= 0 || s.ContainerPort > 65535 {
		return fmt.Errorf("sandboxed module %q: ContainerPort %d out of range", s.Name, s.ContainerPort)
	}
	if s.NetworkIndex < 0 || s.NetworkIndex > SandboxedModuleMaxIndex {
		return fmt.Errorf("sandboxed module %q: NetworkIndex %d out of range [0,%d]", s.Name, s.NetworkIndex, SandboxedModuleMaxIndex)
	}
	for _, sc := range s.Scratch {
		p := strings.TrimSpace(sc.Path)
		if p == "" || !strings.HasPrefix(p, "/") {
			return fmt.Errorf("sandboxed module %q: scratch path %q must be an absolute path", s.Name, sc.Path)
		}
	}
	return nil
}

func validSandboxedModuleName(name string) bool {
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return !strings.HasPrefix(name, "-") && !strings.HasSuffix(name, "-")
}

// --- compose rendering ---

type sandboxComposeDoc struct {
	Services map[string]*sandboxComposeService `yaml:"services"`
	Networks map[string]sandboxComposeNetwork  `yaml:"networks"`
}

type sandboxComposeService struct {
	Image         string                          `yaml:"image"`
	ContainerName string                          `yaml:"container_name"`
	Command       []string                        `yaml:"command,omitempty"`
	ReadOnly      bool                            `yaml:"read_only"`
	CapDrop       []string                        `yaml:"cap_drop"`
	SecurityOpt   []string                        `yaml:"security_opt"`
	PidsLimit     int                             `yaml:"pids_limit"`
	MemLimit      string                          `yaml:"mem_limit"`
	CPUs          string                          `yaml:"cpus"`
	Environment   []string                        `yaml:"environment,omitempty"`
	Volumes       []sandboxTmpfsVolume            `yaml:"volumes"`
	Networks      map[string]sandboxNetworkAttach `yaml:"networks"`
	Restart       string                          `yaml:"restart"`
}

type sandboxTmpfsVolume struct {
	Type   string          `yaml:"type"`
	Target string          `yaml:"target"`
	Tmpfs  sandboxTmpfsOpt `yaml:"tmpfs"`
}

type sandboxTmpfsOpt struct {
	Size int64 `yaml:"size"`
}

type sandboxNetworkAttach struct {
	IPv4Address string `yaml:"ipv4_address"`
}

type sandboxComposeNetwork struct {
	Driver   string                `yaml:"driver"`
	Internal bool                  `yaml:"internal"`
	IPAM     sandboxComposeNetIPAM `yaml:"ipam"`
}

type sandboxComposeNetIPAM struct {
	Config []sandboxComposeNetIPAMConfig `yaml:"config"`
}

type sandboxComposeNetIPAMConfig struct {
	Subnet  string `yaml:"subnet"`
	Gateway string `yaml:"gateway"`
}

// RenderSandboxedModuleCompose renders the compose file that runs spec under the
// sandboxed profile. It is PURE (spec in, YAML string out) and table-tested; the
// enforced controls are identical for every spec by construction.
func RenderSandboxedModuleCompose(spec SandboxedModuleSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}

	mem := strings.TrimSpace(spec.MemoryLimit)
	if mem == "" {
		mem = defaultSandboxedModuleMemory
	}
	cpus := strings.TrimSpace(spec.CPUs)
	if cpus == "" {
		cpus = defaultSandboxedModuleCPUs
	}
	pids := spec.PIDsLimit
	if pids <= 0 {
		pids = defaultSandboxedModulePIDs
	}
	tmpSize := spec.TmpSizeBytes
	if tmpSize <= 0 {
		tmpSize = defaultSandboxedModuleTmpBytes
	}

	// Scratch mounts: /tmp always, plus declared paths (deduped, /tmp not
	// duplicated). Each is a tmpfs volume with NO host source -> no bind mount.
	volumes := []sandboxTmpfsVolume{{Type: "tmpfs", Target: "/tmp", Tmpfs: sandboxTmpfsOpt{Size: tmpSize}}}
	seen := map[string]bool{"/tmp": true}
	for _, sc := range spec.Scratch {
		p := strings.TrimSpace(sc.Path)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		size := sc.SizeBytes
		if size <= 0 {
			size = sandboxedModuleScratchTmpfsSize
		}
		volumes = append(volumes, sandboxTmpfsVolume{Type: "tmpfs", Target: p, Tmpfs: sandboxTmpfsOpt{Size: size}})
	}

	netName := SandboxedModuleNetworkName(spec.Name)

	svc := &sandboxComposeService{
		Image:         spec.Image,
		ContainerName: SandboxedModuleContainerName(spec.Name),
		Command:       spec.Command,
		ReadOnly:      true,
		CapDrop:       []string{SandboxedModuleCapDrop},
		SecurityOpt:   []string{SandboxedModuleNoNewPrivileges},
		PidsLimit:     pids,
		MemLimit:      mem,
		CPUs:          cpus,
		Environment:   sortedEnv(spec.Env),
		Volumes:       volumes,
		Networks: map[string]sandboxNetworkAttach{
			netName: {IPv4Address: SandboxedModuleIP(spec.NetworkIndex)},
		},
		Restart: "unless-stopped",
	}

	doc := sandboxComposeDoc{
		Services: map[string]*sandboxComposeService{spec.Name: svc},
		Networks: map[string]sandboxComposeNetwork{
			netName: {
				Driver:   "bridge",
				Internal: true,
				IPAM: sandboxComposeNetIPAM{Config: []sandboxComposeNetIPAMConfig{{
					Subnet:  SandboxedModuleSubnet(spec.NetworkIndex),
					Gateway: SandboxedModuleGateway(spec.NetworkIndex),
				}}},
			},
		},
	}

	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("sandboxed module %q: marshal compose: %w", spec.Name, err)
	}

	header := "# Citadel generic sandboxed-module runtime profile (auto-generated,\n" +
		"# aceteam-ai/citadel-cli#1288). One least-privilege contract for on-node\n" +
		"# helpers that parse UNTRUSTED input: NO outbound network (internal bridge,\n" +
		"# no default route), host-private static-IP endpoint (never published, never\n" +
		"# off-host, never on the mesh), read-only rootfs, all caps dropped,\n" +
		"# no-new-privileges, pids/mem/cpu capped, tmpfs-only scratch, NO host bind\n" +
		"# mounts. The module image must be self-contained (no runtime network).\n" +
		"# RenderSandboxedModuleCompose (services/sandbox_profile.go) is the single\n" +
		"# authority; do not hand-edit.\n"
	return header + string(data), nil
}

func sortedEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// sandboxedModuleIPInSubnet is a belt-and-suspenders check (used by tests) that
// the static service IP actually lies inside the module's /29 subnet.
func sandboxedModuleIPInSubnet(index int) bool {
	_, ipnet, err := net.ParseCIDR(SandboxedModuleSubnet(index))
	if err != nil {
		return false
	}
	return ipnet.Contains(net.ParseIP(SandboxedModuleIP(index)))
}
