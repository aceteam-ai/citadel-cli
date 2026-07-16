package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeSnippets is an in-memory SnippetStore.
type fakeSnippets struct {
	files map[string]string
}

func newFakeSnippets() *fakeSnippets {
	return &fakeSnippets{files: map[string]string{}}
}

func (f *fakeSnippets) Write(name, content string) (string, error) {
	f.files[name] = content
	return "local:snippets/" + name, nil
}

func (f *fakeSnippets) Remove(name string) error {
	delete(f.files, name)
	return nil
}

// pveMock is a stateful mock PVE API for full provisioning flows.
type pveMock struct {
	t          *testing.T
	pools      map[string][]PoolMember
	cloned     bool
	configForm url.Values
	resized    bool
	started    bool
	stopped    bool
	deleted    bool
	failClone  bool
	failResize bool
}

func (m *pveMock) handler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	switch {
	case r.Method == http.MethodGet && path == "/pools":
		var pools []Pool
		for id := range m.pools {
			pools = append(pools, Pool{PoolID: id})
		}
		fmt.Fprintf(w, `{"data":%s}`, marshalOrDie(m.t, pools))
	case r.Method == http.MethodPost && path == "/pools":
		m.pools[r.PostForm.Get("poolid")] = nil
		w.Write([]byte(`{"data":null}`))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/pools/"):
		id := strings.TrimPrefix(path, "/pools/")
		fmt.Fprintf(w, `{"data":{"members":%s}}`, marshalOrDie(m.t, m.pools[id]))
	case r.Method == http.MethodGet && path == "/cluster/nextid":
		w.Write([]byte(`{"data":"105"}`))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/clone"):
		if m.failClone {
			http.Error(w, `{"errors":{"newid":"already exists"}}`, 400)
			return
		}
		m.cloned = true
		// Cloning with pool= assigns membership.
		if pool := r.PostForm.Get("pool"); pool != "" {
			m.pools[pool] = append(m.pools[pool], PoolMember{Type: "qemu", VMID: 105})
		}
		w.Write([]byte(`{"data":"UPID:pve1:0001:qmclone:"}`))
	case r.Method == http.MethodGet && strings.Contains(path, "/tasks/"):
		w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"OK"}}`))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/config"):
		m.configForm = r.PostForm
		w.Write([]byte(`{"data":null}`))
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/resize"):
		if m.failResize {
			http.Error(w, `{"errors":"disk not found"}`, 400)
			return
		}
		m.resized = true
		w.Write([]byte(`{"data":null}`))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/status/start"):
		m.started = true
		w.Write([]byte(`{"data":null}`))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/status/stop"):
		m.stopped = true
		w.Write([]byte(`{"data":null}`))
	case r.Method == http.MethodDelete && strings.Contains(path, "/qemu/"):
		m.deleted = true
		w.Write([]byte(`{"data":"UPID:pve1:0002:qmdestroy:"}`))
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/status/current"):
		w.Write([]byte(`{"data":{"status":"running","vmid":105,"uptime":42,"cpus":2,"maxmem":4294967296}}`))
	default:
		m.t.Errorf("unexpected request: %s %s", r.Method, path)
		http.Error(w, "not found", 404)
	}
}

func marshalOrDie(t *testing.T, v any) string {
	t.Helper()
	if v == nil {
		return "[]"
	}
	data := wrapData(t, v)
	return strings.TrimSuffix(strings.TrimPrefix(string(data), `{"data":`), "}")
}

func newTestProvisioner(t *testing.T, mock *pveMock, cfg ProvisioningConfig) (*Provisioner, *fakeSnippets) {
	t.Helper()
	orig := taskPollInterval
	taskPollInterval = time.Millisecond
	t.Cleanup(func() { taskPollInterval = orig })

	_, client := newTestServer(t, mock.handler)
	snips := newFakeSnippets()
	if cfg.PVENode == "" {
		cfg.PVENode = "pve1"
	}
	if cfg.TemplateVMID == 0 {
		cfg.TemplateVMID = 9000
	}
	cfg.Enabled = true
	p, err := NewProvisioner(client, cfg, snips, nil)
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	return p, snips
}

func validRequest() ProvisionRequest {
	return ProvisionRequest{
		InstanceID:   "11111111-2222-3333-4444-555555555555",
		Name:         "My Test Box",
		InstanceType: "medium",
		OrgID:        "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		AuthKey:      "hskey-auth-test123",
		SSHAuthorizedKeys: []string{
			"ssh-ed25519 AAAA test@host",
		},
	}
}

func TestProvisionHappyPath(t *testing.T) {
	mock := &pveMock{t: t, pools: map[string][]PoolMember{}}
	p, snips := newTestProvisioner(t, mock, ProvisioningConfig{})

	res, err := p.Provision(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.VMID != 105 || res.PVENode != "pve1" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.Cores != 2 || res.MemoryMB != 4096 || res.DiskGB != 40 {
		t.Errorf("medium tier not applied: %+v", res)
	}
	if res.Name != "my-test-box" {
		t.Errorf("name not sanitized: %q", res.Name)
	}
	if !mock.cloned || !mock.resized || !mock.started {
		t.Errorf("lifecycle incomplete: cloned=%v resized=%v started=%v", mock.cloned, mock.resized, mock.started)
	}

	// Sizing + cloud-init config was applied.
	if mock.configForm.Get("cores") != "2" || mock.configForm.Get("memory") != "4096" {
		t.Errorf("sizing config not applied: %v", mock.configForm)
	}
	cicustom := mock.configForm.Get("cicustom")
	if !strings.HasPrefix(cicustom, "user=local:snippets/aceteam-") {
		t.Errorf("cicustom not set: %q", cicustom)
	}

	// The snippet carries the mesh join with the CUSTOMER org's authkey.
	var content string
	for _, c := range snips.files {
		content = c
	}
	if !strings.Contains(content, "hskey-auth-test123") {
		t.Error("snippet missing authkey")
	}
	if !strings.Contains(content, "--login-server=https://nexus.aceteam.ai") {
		t.Error("snippet missing login server")
	}
	if !strings.Contains(content, "ssh-ed25519 AAAA test@host") {
		t.Error("snippet missing ssh key")
	}
	if !strings.Contains(content, "hostname: my-test-box") {
		t.Error("snippet missing hostname")
	}

	// Pool created for the org.
	pool := PoolForOrg(validRequest().OrgID)
	if _, ok := mock.pools[pool]; !ok {
		t.Errorf("org pool %q not created (pools: %v)", pool, mock.pools)
	}
}

func TestProvisionRejectsUnknownTier(t *testing.T) {
	mock := &pveMock{t: t, pools: map[string][]PoolMember{}}
	p, _ := newTestProvisioner(t, mock, ProvisioningConfig{})
	req := validRequest()
	req.InstanceType = "mega"
	if _, err := p.Provision(context.Background(), req); err == nil || !strings.Contains(err.Error(), "unknown instance_type") {
		t.Fatalf("expected tier error, got %v", err)
	}
}

func TestProvisionRequiresAuthKey(t *testing.T) {
	mock := &pveMock{t: t, pools: map[string][]PoolMember{}}
	p, _ := newTestProvisioner(t, mock, ProvisioningConfig{})
	req := validRequest()
	req.AuthKey = ""
	if _, err := p.Provision(context.Background(), req); err == nil || !strings.Contains(err.Error(), "authkey is required") {
		t.Fatalf("expected authkey error, got %v", err)
	}
}

func TestProvisionEnforcesOrgCap(t *testing.T) {
	req := validRequest()
	pool := PoolForOrg(req.OrgID)
	mock := &pveMock{t: t, pools: map[string][]PoolMember{
		pool: {
			{Type: "qemu", VMID: 101},
			{Type: "qemu", VMID: 102},
		},
	}}
	p, _ := newTestProvisioner(t, mock, ProvisioningConfig{MaxInstancesPerOrg: 2})
	if _, err := p.Provision(context.Background(), req); err == nil || !strings.Contains(err.Error(), "instance cap") {
		t.Fatalf("expected cap error, got %v", err)
	}
	if mock.cloned {
		t.Error("clone must not run when the org is at cap")
	}
}

func TestProvisionRollsBackOnResizeFailure(t *testing.T) {
	mock := &pveMock{t: t, pools: map[string][]PoolMember{}, failResize: true}
	p, snips := newTestProvisioner(t, mock, ProvisioningConfig{})
	if _, err := p.Provision(context.Background(), validRequest()); err == nil {
		t.Fatal("expected resize failure")
	}
	if !mock.deleted {
		t.Error("expected best-effort VM cleanup after failure")
	}
	if len(snips.files) != 0 {
		t.Error("expected snippet cleanup after failure")
	}
}

func TestProvisionCleansSnippetOnCloneFailure(t *testing.T) {
	mock := &pveMock{t: t, pools: map[string][]PoolMember{}, failClone: true}
	p, snips := newTestProvisioner(t, mock, ProvisioningConfig{})
	if _, err := p.Provision(context.Background(), validRequest()); err == nil {
		t.Fatal("expected clone failure")
	}
	if len(snips.files) != 0 {
		t.Error("expected snippet cleanup after clone failure")
	}
	if mock.deleted {
		t.Error("no VM to delete when clone itself failed")
	}
}

func TestDestroyStopsDeletesAndRemovesSnippet(t *testing.T) {
	mock := &pveMock{t: t, pools: map[string][]PoolMember{}}
	p, snips := newTestProvisioner(t, mock, ProvisioningConfig{})
	snips.files[snippetName("inst-1")] = "content"

	if err := p.Destroy(context.Background(), InstanceRef{VMID: 105}, "inst-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !mock.stopped || !mock.deleted {
		t.Errorf("expected stop+delete, got stopped=%v deleted=%v", mock.stopped, mock.deleted)
	}
	if len(snips.files) != 0 {
		t.Error("expected snippet removal")
	}
}

func TestStatus(t *testing.T) {
	mock := &pveMock{t: t, pools: map[string][]PoolMember{}}
	p, _ := newTestProvisioner(t, mock, ProvisioningConfig{})
	st, err := p.Status(context.Background(), InstanceRef{VMID: 105})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Status != "running" || st.UptimeSeconds != 42 {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestNewProvisionerValidation(t *testing.T) {
	client := NewClient(ClientConfig{BaseURL: "https://example:8006"})
	cases := []struct {
		name string
		cfg  ProvisioningConfig
		want string
	}{
		{"disabled", ProvisioningConfig{}, "not enabled"},
		{"no template", ProvisioningConfig{Enabled: true, PVENode: "pve1"}, "template_vmid"},
		{"no node", ProvisioningConfig{Enabled: true, TemplateVMID: 9000}, "pve_node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProvisioner(client, tc.cfg, newFakeSnippets(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestPoolForOrg(t *testing.T) {
	got := PoolForOrg("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if got != "aceteam-org-aaaaaaaabbbb" {
		t.Errorf("unexpected pool name: %q", got)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"My Test Box":  "my-test-box",
		"  weird__x  ": "weird-x",
		"":             "instance",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
