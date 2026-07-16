// internal/proxmox/provisioner.go
//
// Provisioner turns fabric INSTANCE_* jobs (aceteam#5963) into Proxmox VE API
// calls: clone a cloud-init template, size the VM from a small instance-type
// table, inject an org-scoped mesh authkey via a cloud-init user-data snippet,
// group the customer org's instances into a PVE resource pool (used for the
// per-org instance cap), and drive start/stop/destroy/status.
//
// Mesh delivery: the platform mints a headscale authkey for the CUSTOMER org
// (org_<id>), the provision payload carries it here, and the rendered
// cloud-config installs tailscale on first boot and joins the mesh with that
// key -- so the instance lands in the customer's namespace, not the
// operator's. Renewal/revocation composes with the device-identity work
// (aceteam#5959); once its device-profile leaf issuance ships, first boot
// should additionally enroll a device identity so mesh membership becomes
// derived state. TODO(aceteam#5959): swap the one-shot authkey for the
// device-identity enrolment flow when available.
//
// Snippet delivery assumes the citadel node runs ON the PVE host (or has the
// snippets directory mounted): the PVE API cannot upload snippet files itself.
package proxmox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ProvisioningConfig configures instance provisioning on a PVE host. It lives
// under the "provisioning" key of proxmox.json.
type ProvisioningConfig struct {
	// Enabled gates the whole INSTANCE_* job family on this node.
	Enabled bool `json:"enabled"`
	// PVENode is the PVE node name to provision on (defaults to Config.NodeName).
	PVENode string `json:"pve_node,omitempty"`
	// TemplateVMID is the cloud-init template VM to clone from. Required.
	TemplateVMID int `json:"template_vmid"`
	// Storage is the target storage for full clones (optional; PVE default).
	Storage string `json:"storage,omitempty"`
	// DiskDevice is the boot disk to resize (default "scsi0").
	DiskDevice string `json:"disk_device,omitempty"`
	// SnippetsDir is the local directory backing the snippets content type of
	// SnippetsStorage (default "/var/lib/vz/snippets").
	SnippetsDir string `json:"snippets_dir,omitempty"`
	// SnippetsStorage is the PVE storage id that serves SnippetsDir (default "local").
	SnippetsStorage string `json:"snippets_storage,omitempty"`
	// LoginServer is the mesh coordination server new instances join
	// (default "https://nexus.aceteam.ai").
	LoginServer string `json:"login_server,omitempty"`
	// MaxInstancesPerOrg caps instances per customer org, enforced via the
	// org's PVE resource pool (default 3; 0 uses the default, -1 disables).
	MaxInstancesPerOrg int `json:"max_instances_per_org,omitempty"`
}

const (
	defaultDiskDevice         = "scsi0"
	defaultSnippetsDir        = "/var/lib/vz/snippets"
	defaultSnippetsStorage    = "local"
	defaultLoginServer        = "https://nexus.aceteam.ai"
	defaultMaxInstancesPerOrg = 3
	provisionTaskTimeout      = 5 * time.Minute
)

// InstanceType describes one row of the instance sizing table.
type InstanceType struct {
	Name     string
	Cores    int
	MemoryMB int
	DiskGB   int
}

// InstanceTypes is the built-in sizing table for fabric instances. It mirrors
// the platform's tier table (models/fabric_instance.py in aceteam); keep the
// two in sync.
var InstanceTypes = map[string]InstanceType{
	"small":  {Name: "small", Cores: 1, MemoryMB: 2048, DiskGB: 20},
	"medium": {Name: "medium", Cores: 2, MemoryMB: 4096, DiskGB: 40},
	"large":  {Name: "large", Cores: 4, MemoryMB: 8192, DiskGB: 80},
	"xlarge": {Name: "xlarge", Cores: 8, MemoryMB: 16384, DiskGB: 160},
}

// ProvisionRequest is the input for provisioning one instance.
type ProvisionRequest struct {
	// InstanceID is the platform's instance UUID (used in snippet/tag names).
	InstanceID string
	// Name is the requested instance name (sanitized to DNS-safe).
	Name string
	// InstanceType selects a row of InstanceTypes.
	InstanceType string
	// OrgID is the CUSTOMER org UUID the instance belongs to.
	OrgID string
	// AuthKey is the org-scoped mesh authkey injected at first boot. Required.
	AuthKey string
	// LoginServer overrides the configured mesh coordination server (optional).
	LoginServer string
	// SSHAuthorizedKeys are added to the default user via cloud-init (optional).
	SSHAuthorizedKeys []string
}

// ProvisionResult reports a successfully provisioned instance.
type ProvisionResult struct {
	VMID     int    `json:"vmid"`
	PVENode  string `json:"pve_node"`
	Name     string `json:"name"`
	Cores    int    `json:"cores"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
	Pool     string `json:"pool"`
}

// InstanceRef identifies an existing instance VM.
type InstanceRef struct {
	VMID    int
	PVENode string
}

// InstanceStatus reports the live state of an instance VM.
type InstanceStatus struct {
	VMID          int    `json:"vmid"`
	Status        string `json:"status"` // "running" | "stopped"
	UptimeSeconds int64  `json:"uptime_seconds"`
	CPUs          int    `json:"cpus"`
	MaxMemBytes   int64  `json:"max_mem_bytes"`
}

// SnippetStore persists cloud-init user-data snippets where the PVE storage
// layer can see them. The live implementation writes to the snippets directory
// on the PVE host; tests use an in-memory fake.
type SnippetStore interface {
	// Write stores content under the given file name and returns the PVE
	// volume reference (e.g. "local:snippets/aceteam-<id>.yaml").
	Write(name, content string) (string, error)
	// Remove deletes a snippet by file name. Missing files are not an error.
	Remove(name string) error
}

// LocalSnippetStore writes snippets to a directory on the local filesystem
// (the PVE host's snippets dir, when citadel runs on the hypervisor).
type LocalSnippetStore struct {
	Dir     string // e.g. /var/lib/vz/snippets
	Storage string // e.g. "local"
}

// Write implements SnippetStore.
func (s *LocalSnippetStore) Write(name, content string) (string, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", fmt.Errorf("creating snippets dir: %w", err)
	}
	path := filepath.Join(s.Dir, name)
	// 0600: the snippet carries a mesh authkey; only root on the hypervisor
	// (which already owns the VMs) should read it.
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("writing snippet: %w", err)
	}
	return fmt.Sprintf("%s:snippets/%s", s.Storage, name), nil
}

// Remove implements SnippetStore.
func (s *LocalSnippetStore) Remove(name string) error {
	err := os.Remove(filepath.Join(s.Dir, name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Provisioner orchestrates instance provisioning against one PVE host.
type Provisioner struct {
	client   *Client
	cfg      ProvisioningConfig
	snippets SnippetStore
	log      func(format string, args ...any)
}

// NewProvisioner builds a Provisioner. cfg.TemplateVMID and cfg.PVENode must be
// set (PVENode may be defaulted by the caller from the connection config).
func NewProvisioner(client *Client, cfg ProvisioningConfig, snippets SnippetStore, log func(format string, args ...any)) (*Provisioner, error) {
	if client == nil {
		return nil, fmt.Errorf("proxmox client is required")
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("instance provisioning is not enabled on this node")
	}
	if cfg.TemplateVMID <= 0 {
		return nil, fmt.Errorf("provisioning.template_vmid is required")
	}
	if cfg.PVENode == "" {
		return nil, fmt.Errorf("provisioning.pve_node is required")
	}
	if cfg.DiskDevice == "" {
		cfg.DiskDevice = defaultDiskDevice
	}
	if cfg.LoginServer == "" {
		cfg.LoginServer = defaultLoginServer
	}
	if cfg.MaxInstancesPerOrg == 0 {
		cfg.MaxInstancesPerOrg = defaultMaxInstancesPerOrg
	}
	if snippets == nil {
		dir := cfg.SnippetsDir
		if dir == "" {
			dir = defaultSnippetsDir
		}
		storage := cfg.SnippetsStorage
		if storage == "" {
			storage = defaultSnippetsStorage
		}
		snippets = &LocalSnippetStore{Dir: dir, Storage: storage}
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Provisioner{client: client, cfg: cfg, snippets: snippets, log: log}, nil
}

// PoolForOrg returns the PVE resource pool name for a customer org.
// PVE pool ids allow [A-Za-z0-9\-_.]; org UUIDs are shortened for readability.
func PoolForOrg(orgID string) string {
	id := strings.ReplaceAll(orgID, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	return "aceteam-org-" + id
}

var nameSanitizer = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeName makes a DNS-safe VM name.
func sanitizeName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = nameSanitizer.ReplaceAllString(n, "-")
	n = strings.Trim(n, "-")
	if len(n) > 48 {
		n = n[:48]
	}
	if n == "" {
		n = "instance"
	}
	return n
}

// snippetName returns the user-data snippet file name for an instance.
func snippetName(instanceID string) string {
	return "aceteam-" + sanitizeName(instanceID) + ".yaml"
}

// renderCloudInit renders the #cloud-config user-data that joins the instance
// to the customer org's mesh namespace on first boot.
func renderCloudInit(hostname, loginServer, authKey string, sshKeys []string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %s\n", hostname)
	b.WriteString("manage_etc_hosts: true\n")
	if len(sshKeys) > 0 {
		b.WriteString("ssh_authorized_keys:\n")
		for _, k := range sshKeys {
			fmt.Fprintf(&b, "  - %s\n", strings.TrimSpace(k))
		}
	}
	b.WriteString("package_update: true\n")
	b.WriteString("runcmd:\n")
	b.WriteString("  - ['sh', '-c', 'curl -fsSL https://tailscale.com/install.sh | sh']\n")
	fmt.Fprintf(&b, "  - ['tailscale', 'up', '--login-server=%s', '--authkey=%s', '--hostname=%s']\n",
		loginServer, authKey, hostname)
	return b.String()
}

// Provision clones, sizes, mesh-enrolls, pools, and starts a new instance VM.
func (p *Provisioner) Provision(ctx context.Context, req ProvisionRequest) (*ProvisionResult, error) {
	tier, ok := InstanceTypes[strings.ToLower(strings.TrimSpace(req.InstanceType))]
	if !ok {
		return nil, fmt.Errorf("unknown instance_type %q (want one of small|medium|large|xlarge)", req.InstanceType)
	}
	if req.OrgID == "" {
		return nil, fmt.Errorf("org_id is required")
	}
	if req.AuthKey == "" {
		return nil, fmt.Errorf("authkey is required: an instance without a mesh identity is unreachable")
	}
	if req.InstanceID == "" {
		return nil, fmt.Errorf("instance_id is required")
	}

	// Per-org cap, enforced via the org's resource pool.
	pool := PoolForOrg(req.OrgID)
	if _, err := p.client.EnsurePool(ctx, pool, "AceTeam fabric instances for org "+req.OrgID); err != nil {
		return nil, err
	}
	if p.cfg.MaxInstancesPerOrg > 0 {
		members, err := p.client.GetPoolMembers(ctx, pool)
		if err != nil {
			return nil, fmt.Errorf("checking org instance cap: %w", err)
		}
		count := 0
		for _, m := range members {
			if m.Type == "qemu" {
				count++
			}
		}
		if count >= p.cfg.MaxInstancesPerOrg {
			return nil, fmt.Errorf("org %s is at its instance cap (%d) on this node", req.OrgID, p.cfg.MaxInstancesPerOrg)
		}
	}

	vmid, err := p.client.NextID(ctx)
	if err != nil {
		return nil, fmt.Errorf("allocating vmid: %w", err)
	}
	name := sanitizeName(req.Name)

	// Cloud-init user-data snippet: mesh join at first boot.
	loginServer := req.LoginServer
	if loginServer == "" {
		loginServer = p.cfg.LoginServer
	}
	snipName := snippetName(req.InstanceID)
	volRef, err := p.snippets.Write(snipName, renderCloudInit(name, loginServer, req.AuthKey, req.SSHAuthorizedKeys))
	if err != nil {
		return nil, fmt.Errorf("writing cloud-init snippet: %w", err)
	}

	p.log("INSTANCE_PROVISION: cloning template %d -> vmid %d (%s, %s)", p.cfg.TemplateVMID, vmid, name, tier.Name)
	upid, err := p.client.CloneVM(ctx, p.cfg.PVENode, p.cfg.TemplateVMID, vmid, CloneOptions{
		Name:    name,
		Full:    true,
		Storage: p.cfg.Storage,
		Pool:    pool,
	})
	if err != nil {
		_ = p.snippets.Remove(snipName)
		return nil, fmt.Errorf("cloning template: %w", err)
	}
	if err := p.client.WaitForTask(ctx, p.cfg.PVENode, upid, provisionTaskTimeout); err != nil {
		_ = p.snippets.Remove(snipName)
		return nil, fmt.Errorf("clone task: %w", err)
	}

	// From here on, failures leave a cloned VM behind; clean it up best-effort.
	fail := func(err error) (*ProvisionResult, error) {
		p.log("INSTANCE_PROVISION: rolling back vmid %d after failure: %v", vmid, err)
		p.cleanup(ctx, vmid, snipName)
		return nil, err
	}

	if err := p.client.ConfigureVM(ctx, p.cfg.PVENode, vmid, map[string]string{
		"cores":       strconv.Itoa(tier.Cores),
		"memory":      strconv.Itoa(tier.MemoryMB),
		"cicustom":    "user=" + volRef,
		"ipconfig0":   "ip=dhcp",
		"tags":        "aceteam;" + PoolForOrg(req.OrgID),
		"description": fmt.Sprintf("AceTeam fabric instance %s (org %s, %s)", req.InstanceID, req.OrgID, tier.Name),
	}); err != nil {
		return fail(fmt.Errorf("configuring VM: %w", err))
	}

	if err := p.client.ResizeDisk(ctx, p.cfg.PVENode, vmid, p.cfg.DiskDevice, fmt.Sprintf("%dG", tier.DiskGB)); err != nil {
		return fail(fmt.Errorf("resizing disk: %w", err))
	}

	if err := p.client.StartGuest(ctx, p.cfg.PVENode, "qemu", vmid); err != nil {
		return fail(fmt.Errorf("starting VM: %w", err))
	}

	return &ProvisionResult{
		VMID:     vmid,
		PVENode:  p.cfg.PVENode,
		Name:     name,
		Cores:    tier.Cores,
		MemoryMB: tier.MemoryMB,
		DiskGB:   tier.DiskGB,
		Pool:     pool,
	}, nil
}

// cleanup best-effort destroys a partially provisioned VM and its snippet.
func (p *Provisioner) cleanup(ctx context.Context, vmid int, snipName string) {
	_ = p.client.StopGuest(ctx, p.cfg.PVENode, "qemu", vmid)
	if upid, err := p.client.DeleteVM(ctx, p.cfg.PVENode, vmid, true); err == nil {
		_ = p.client.WaitForTask(ctx, p.cfg.PVENode, upid, provisionTaskTimeout)
	}
	_ = p.snippets.Remove(snipName)
}

// Start starts an instance VM.
func (p *Provisioner) Start(ctx context.Context, ref InstanceRef) error {
	return p.client.StartGuest(ctx, p.node(ref), "qemu", ref.VMID)
}

// Stop force-stops an instance VM.
func (p *Provisioner) Stop(ctx context.Context, ref InstanceRef) error {
	return p.client.StopGuest(ctx, p.node(ref), "qemu", ref.VMID)
}

// Destroy stops (best-effort) and deletes an instance VM plus its snippet.
func (p *Provisioner) Destroy(ctx context.Context, ref InstanceRef, instanceID string) error {
	node := p.node(ref)
	_ = p.client.StopGuest(ctx, node, "qemu", ref.VMID)
	upid, err := p.client.DeleteVM(ctx, node, ref.VMID, true)
	if err != nil {
		return fmt.Errorf("deleting VM %d: %w", ref.VMID, err)
	}
	if err := p.client.WaitForTask(ctx, node, upid, provisionTaskTimeout); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	if instanceID != "" {
		_ = p.snippets.Remove(snippetName(instanceID))
	}
	return nil
}

// Status returns the live status of an instance VM.
func (p *Provisioner) Status(ctx context.Context, ref InstanceRef) (*InstanceStatus, error) {
	st, err := p.client.GetGuestStatus(ctx, p.node(ref), "qemu", ref.VMID)
	if err != nil {
		return nil, err
	}
	return &InstanceStatus{
		VMID:          ref.VMID,
		Status:        st.Status,
		UptimeSeconds: st.Uptime,
		CPUs:          st.CPUs,
		MaxMemBytes:   st.MaxMem,
	}, nil
}

func (p *Provisioner) node(ref InstanceRef) string {
	if ref.PVENode != "" {
		return ref.PVENode
	}
	return p.cfg.PVENode
}
