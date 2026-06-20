package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- Mock Docker Backend ---

type mockBackend struct {
	mu          sync.Mutex
	containers  map[string]ResourceStatus
	createErr   error
	destroyErr  error
	logsOutput  string
	createCalls int
}

func newMockBackend() *mockBackend {
	return &mockBackend{containers: make(map[string]ResourceStatus)}
}

func (m *mockBackend) Create(_ context.Context, id string, _ *ResourceSpec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	if m.createErr != nil {
		return "", m.createErr
	}
	m.containers[id] = StatusRunning
	return "mock-cid-" + id[:8], nil
}

func (m *mockBackend) Destroy(_ context.Context, id string, _ *ResourceSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.destroyErr != nil {
		return m.destroyErr
	}
	m.containers[id] = StatusDestroyed
	return nil
}

func (m *mockBackend) Inspect(_ context.Context, id string, _ *ResourceSpec) (ResourceStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.containers[id]; ok {
		return s, nil
	}
	return StatusDestroyed, nil
}

func (m *mockBackend) Logs(_ context.Context, _ string, _ *ResourceSpec, _ int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logsOutput, nil
}

// --- Store Tests ---

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	r := &Resource{
		ID:     "test-id-1",
		Spec:   ResourceSpec{Name: "test-res", Type: ResourceTypeDocker, Image: "nginx"},
		Status: StatusRunning,
	}
	if err := store.Put(r); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "test-id-1") {
		t.Fatalf("store file does not contain resource ID: %s", data)
	}

	store2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := store2.Get("test-id-1")
	if got == nil {
		t.Fatal("expected resource after reload, got nil")
	}
	if got.Spec.Name != "test-res" {
		t.Fatalf("expected name test-res, got %s", got.Spec.Name)
	}
}

func TestStoreFindByName(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	store.Put(&Resource{ID: "id-1", Spec: ResourceSpec{Name: "alpha"}, Status: StatusRunning})
	store.Put(&Resource{ID: "id-2", Spec: ResourceSpec{Name: "beta"}, Status: StatusRunning})

	if got := store.FindByName("beta"); got == nil || got.ID != "id-2" {
		t.Fatalf("FindByName(beta) = %v, want id-2", got)
	}
	if got := store.FindByName("gamma"); got != nil {
		t.Fatalf("FindByName(gamma) = %v, want nil", got)
	}
}

func TestStoreDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	store.Put(&Resource{ID: "id-del", Spec: ResourceSpec{Name: "to-delete"}, Status: StatusRunning})
	if err := store.Delete("id-del"); err != nil {
		t.Fatal(err)
	}
	if got := store.Get("id-del"); got != nil {
		t.Fatalf("expected nil after delete, got %v", got)
	}

	store2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store2.Get("id-del"); got != nil {
		t.Fatalf("expected nil after reload, got %v", got)
	}
}

func TestStoreEmptyLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.List()) != 0 {
		t.Fatal("expected empty store")
	}
}

// --- Validation Tests ---

func TestValidateSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    ResourceSpec
		wantErr string
	}{
		{
			name:    "empty name",
			spec:    ResourceSpec{Type: ResourceTypeDocker, Image: "nginx"},
			wantErr: "name is required",
		},
		{
			name:    "invalid name",
			spec:    ResourceSpec{Name: "bad name!", Type: ResourceTypeDocker, Image: "nginx"},
			wantErr: "invalid name",
		},
		{
			name:    "name starting with dot",
			spec:    ResourceSpec{Name: ".hidden", Type: ResourceTypeDocker, Image: "nginx"},
			wantErr: "invalid name",
		},
		{
			name:    "unsupported lxc",
			spec:    ResourceSpec{Name: "test", Type: ResourceTypeLXC, Image: "ubuntu"},
			wantErr: "not yet supported",
		},
		{
			name:    "unknown type",
			spec:    ResourceSpec{Name: "test", Type: "magic"},
			wantErr: "unknown resource type",
		},
		{
			name:    "missing image",
			spec:    ResourceSpec{Name: "test", Type: ResourceTypeDocker},
			wantErr: "image is required",
		},
		{
			name: "invalid host port",
			spec: ResourceSpec{
				Name: "test", Type: ResourceTypeDocker, Image: "nginx",
				Ports: []PortMapping{{HostPort: 0, ContainerPort: 80}},
			},
			wantErr: "invalid host port",
		},
		{
			name: "invalid protocol",
			spec: ResourceSpec{
				Name: "test", Type: ResourceTypeDocker, Image: "nginx",
				Ports: []PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "icmp"}},
			},
			wantErr: "invalid protocol",
		},
		{
			name: "valid spec",
			spec: ResourceSpec{
				Name: "my-app", Type: ResourceTypeDocker, Image: "nginx:latest",
				Ports: []PortMapping{{HostPort: 8080, ContainerPort: 80}},
				Env:   map[string]string{"FOO": "bar"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSpec(&tt.spec)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateVolumePath(t *testing.T) {
	old := allowedVolumePrefixes
	defer func() { allowedVolumePrefixes = old }()
	SetAllowedVolumePrefixes([]string{"/tmp/", "/var/lib/citadel/volumes/"})

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "allowed /tmp", path: "/tmp/data"},
		{name: "allowed citadel volumes", path: "/var/lib/citadel/volumes/mydata"},
		{name: "relative path", path: "data", wantErr: "must be absolute"},
		{name: "not allowed", path: "/etc/shadow", wantErr: "not under an allowed prefix"},
		{name: "empty", path: "", wantErr: "host_path is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVolumePath(tt.path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// --- Manager Tests ---

func newTestManager(t *testing.T, backend *mockBackend) *Manager {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(store, map[ResourceType]Backend{ResourceTypeDocker: backend})
}

func TestManagerCreateAndStatus(t *testing.T) {
	backend := newMockBackend()
	mgr := newTestManager(t, backend)
	ctx := context.Background()

	result, err := mgr.Create(ctx, &ResourceSpec{Name: "test-app", Type: ResourceTypeDocker, Image: "nginx:latest"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reused {
		t.Fatal("expected new resource, got reused")
	}
	if result.Resource.Status != StatusRunning {
		t.Fatalf("expected running, got %s", result.Resource.Status)
	}
	if result.Resource.ContainerID == "" {
		t.Fatal("expected container ID")
	}

	r, err := mgr.Status(ctx, result.Resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusRunning {
		t.Fatalf("expected running, got %s", r.Status)
	}
}

func TestManagerCreateIdempotent(t *testing.T) {
	backend := newMockBackend()
	mgr := newTestManager(t, backend)
	ctx := context.Background()

	spec := &ResourceSpec{Name: "idem-app", Type: ResourceTypeDocker, Image: "nginx"}
	r1, err := mgr.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mgr.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Reused {
		t.Fatal("expected reused=true")
	}
	if r2.Resource.ID != r1.Resource.ID {
		t.Fatal("expected same resource ID")
	}
	if backend.createCalls != 1 {
		t.Fatalf("expected 1 backend create, got %d", backend.createCalls)
	}
}

func TestManagerDestroy(t *testing.T) {
	backend := newMockBackend()
	mgr := newTestManager(t, backend)
	ctx := context.Background()

	result, _ := mgr.Create(ctx, &ResourceSpec{Name: "to-destroy", Type: ResourceTypeDocker, Image: "nginx"})
	if err := mgr.Destroy(ctx, result.Resource.ID); err != nil {
		t.Fatal(err)
	}
	if r := mgr.Get(result.Resource.ID); r.Status != StatusDestroyed {
		t.Fatalf("expected destroyed, got %s", r.Status)
	}
}

func TestManagerDestroyNotFound(t *testing.T) {
	backend := newMockBackend()
	mgr := newTestManager(t, backend)
	if err := mgr.Destroy(context.Background(), "nonexistent"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestManagerCreateBackendError(t *testing.T) {
	backend := newMockBackend()
	backend.createErr = fmt.Errorf("docker daemon not running")
	mgr := newTestManager(t, backend)

	_, err := mgr.Create(context.Background(), &ResourceSpec{Name: "will-fail", Type: ResourceTypeDocker, Image: "nginx"})
	if err == nil || !strings.Contains(err.Error(), "docker daemon") {
		t.Fatalf("expected docker error, got %v", err)
	}
	if resources := mgr.List(); len(resources) != 1 || resources[0].Status != StatusError {
		t.Fatalf("expected 1 resource with error status, got %v", resources)
	}
}

func TestManagerList(t *testing.T) {
	backend := newMockBackend()
	mgr := newTestManager(t, backend)
	ctx := context.Background()

	for _, name := range []string{"app-1", "app-2", "app-3"} {
		mgr.Create(ctx, &ResourceSpec{Name: name, Type: ResourceTypeDocker, Image: "nginx"})
	}
	if got := len(mgr.List()); got != 3 {
		t.Fatalf("expected 3 resources, got %d", got)
	}
}

func TestManagerReconcileAll(t *testing.T) {
	backend := newMockBackend()
	mgr := newTestManager(t, backend)
	ctx := context.Background()

	result, _ := mgr.Create(ctx, &ResourceSpec{Name: "reconcile-me", Type: ResourceTypeDocker, Image: "nginx"})

	backend.mu.Lock()
	backend.containers[result.Resource.ID] = StatusStopped
	backend.mu.Unlock()

	mgr.ReconcileAll(ctx)

	if r := mgr.Get(result.Resource.ID); r.Status != StatusStopped {
		t.Fatalf("expected stopped, got %s", r.Status)
	}
}

func TestManagerLogs(t *testing.T) {
	backend := newMockBackend()
	backend.logsOutput = "line1\nline2\n"
	mgr := newTestManager(t, backend)
	ctx := context.Background()

	result, _ := mgr.Create(ctx, &ResourceSpec{Name: "log-app", Type: ResourceTypeDocker, Image: "nginx"})
	logs, err := mgr.Logs(ctx, result.Resource.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if logs != "line1\nline2\n" {
		t.Fatalf("unexpected logs: %q", logs)
	}
}

func TestManagerCreateAfterDestroyed(t *testing.T) {
	backend := newMockBackend()
	mgr := newTestManager(t, backend)
	ctx := context.Background()

	spec := &ResourceSpec{Name: "phoenix", Type: ResourceTypeDocker, Image: "nginx"}
	r1, _ := mgr.Create(ctx, spec)
	mgr.Destroy(ctx, r1.Resource.ID)

	r2, err := mgr.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Reused {
		t.Fatal("expected new resource after destroy")
	}
	if r2.Resource.ID == r1.Resource.ID {
		t.Fatal("expected different ID after destroy + re-create")
	}
}

// --- HTTP Handler Tests ---

func noAuthMiddleware(next http.HandlerFunc) http.HandlerFunc { return next }

func setupTestHandler(t *testing.T) (*Handler, *Manager, *mockBackend) {
	t.Helper()
	backend := newMockBackend()
	mgr := newTestManager(t, backend)
	return NewHandler(mgr), mgr, backend
}

func TestHTTPCreate(t *testing.T) {
	handler, _, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	body := `{"name":"http-test","type":"docker","image":"nginx:latest"}`
	req := httptest.NewRequest(http.MethodPost, "/provision/create", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var result CreateResult
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.Resource.Spec.Name != "http-test" {
		t.Fatalf("expected name http-test, got %s", result.Resource.Spec.Name)
	}
}

func TestHTTPCreateInvalid(t *testing.T) {
	handler, _, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	req := httptest.NewRequest(http.MethodPost, "/provision/create", strings.NewReader(`{"name":"invalid","type":"docker"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHTTPList(t *testing.T) {
	handler, mgr, _ := setupTestHandler(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	mgr.Create(ctx, &ResourceSpec{Name: "list-1", Type: ResourceTypeDocker, Image: "nginx"})
	mgr.Create(ctx, &ResourceSpec{Name: "list-2", Type: ResourceTypeDocker, Image: "redis"})

	req := httptest.NewRequest(http.MethodGet, "/provision/list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["count"].(float64)) != 2 {
		t.Fatalf("expected count 2, got %v", resp["count"])
	}
}

func TestHTTPStatus(t *testing.T) {
	handler, mgr, _ := setupTestHandler(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	result, _ := mgr.Create(ctx, &ResourceSpec{Name: "status-test", Type: ResourceTypeDocker, Image: "nginx"})

	req := httptest.NewRequest(http.MethodGet, "/provision/"+result.Resource.ID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHTTPStatusNotFound(t *testing.T) {
	handler, _, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	req := httptest.NewRequest(http.MethodGet, "/provision/nonexistent-id", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHTTPDestroy(t *testing.T) {
	handler, mgr, _ := setupTestHandler(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	result, _ := mgr.Create(ctx, &ResourceSpec{Name: "destroy-test", Type: ResourceTypeDocker, Image: "nginx"})

	req := httptest.NewRequest(http.MethodPost, "/provision/"+result.Resource.ID+"/destroy", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHTTPLogs(t *testing.T) {
	handler, mgr, backend := setupTestHandler(t)
	ctx := context.Background()
	backend.logsOutput = "hello world\n"
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	result, _ := mgr.Create(ctx, &ResourceSpec{Name: "logs-test", Type: ResourceTypeDocker, Image: "nginx"})

	req := httptest.NewRequest(http.MethodGet, "/provision/"+result.Resource.ID+"/logs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello world") {
		t.Fatalf("expected logs to contain 'hello world', got %s", w.Body.String())
	}
}

func TestHTTPListEmpty(t *testing.T) {
	handler, _, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, noAuthMiddleware)

	req := httptest.NewRequest(http.MethodGet, "/provision/list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["count"].(float64)) != 0 {
		t.Fatalf("expected count 0, got %v", resp["count"])
	}
}
