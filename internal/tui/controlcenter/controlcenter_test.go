package controlcenter

import (
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"

	"github.com/aceteam-ai/citadel-cli/internal/ui"
)

// TestJobRecordStruct tests the JobRecord struct fields
func TestJobRecordStruct(t *testing.T) {
	now := time.Now()
	record := JobRecord{
		ID:          "job-123",
		Type:        "llm_inference",
		Status:      "success",
		StartedAt:   now,
		CompletedAt: now.Add(time.Second),
		Duration:    time.Second,
		Error:       "",
	}

	if record.ID != "job-123" {
		t.Errorf("JobRecord.ID = %s, want job-123", record.ID)
	}
	if record.Type != "llm_inference" {
		t.Errorf("JobRecord.Type = %s, want llm_inference", record.Type)
	}
	if record.Status != "success" {
		t.Errorf("JobRecord.Status = %s, want success", record.Status)
	}
	if record.Duration != time.Second {
		t.Errorf("JobRecord.Duration = %v, want 1s", record.Duration)
	}
}

// TestQueueInfoStruct tests the QueueInfo struct fields
func TestQueueInfoStruct(t *testing.T) {
	queue := QueueInfo{
		Name:         "jobs:v1:gpu-general",
		Type:         "redis",
		Connected:    true,
		PendingCount: 5,
	}

	if queue.Name != "jobs:v1:gpu-general" {
		t.Errorf("QueueInfo.Name = %s, want jobs:v1:gpu-general", queue.Name)
	}
	if queue.Type != "redis" {
		t.Errorf("QueueInfo.Type = %s, want redis", queue.Type)
	}
	if !queue.Connected {
		t.Error("QueueInfo.Connected = false, want true")
	}
	if queue.PendingCount != 5 {
		t.Errorf("QueueInfo.PendingCount = %d, want 5", queue.PendingCount)
	}
}

// TestStatusDataNewFields tests the new fields in StatusData
func TestStatusDataNewFields(t *testing.T) {
	data := StatusData{
		NodeName:  "test-node",
		NodeIP:    "100.64.0.1",
		Connected: true,
		UserEmail: "test@example.com",
		UserName:  "Test User",
		Queues: []QueueInfo{
			{Name: "queue1", Type: "redis", Connected: true, PendingCount: 10},
			{Name: "queue2", Type: "api", Connected: false, PendingCount: 0},
		},
		RecentJobs: []JobRecord{
			{ID: "job-1", Type: "test", Status: "success"},
			{ID: "job-2", Type: "test", Status: "failed"},
		},
	}

	if data.UserEmail != "test@example.com" {
		t.Errorf("StatusData.UserEmail = %s, want test@example.com", data.UserEmail)
	}
	if data.UserName != "Test User" {
		t.Errorf("StatusData.UserName = %s, want Test User", data.UserName)
	}
	if len(data.Queues) != 2 {
		t.Errorf("len(StatusData.Queues) = %d, want 2", len(data.Queues))
	}
	if len(data.RecentJobs) != 2 {
		t.Errorf("len(StatusData.RecentJobs) = %d, want 2", len(data.RecentJobs))
	}
}

// TestRecordJob tests that RecordJob keeps only the last 10 jobs
func TestRecordJob(t *testing.T) {
	cc := &ControlCenter{
		recentJobs: make([]JobRecord, 0),
	}

	// Add 15 jobs
	for i := 0; i < 15; i++ {
		record := JobRecord{
			ID:        "job-" + string(rune('a'+i)),
			Type:      "test",
			Status:    "success",
			StartedAt: time.Now(),
			Duration:  time.Second,
		}
		cc.RecordJob(record)
	}

	// Should only keep last 10
	if len(cc.recentJobs) != 10 {
		t.Errorf("len(recentJobs) = %d, want 10", len(cc.recentJobs))
	}

	// Most recent job should be first (newest first ordering)
	// The last added job was "job-o" (15th job, i=14)
	if cc.recentJobs[0].ID != "job-o" {
		t.Errorf("recentJobs[0].ID = %s, want job-o", cc.recentJobs[0].ID)
	}
}

// TestRecordJobPrependsNewest tests that newest jobs are prepended
func TestRecordJobPrependsNewest(t *testing.T) {
	cc := &ControlCenter{
		recentJobs: make([]JobRecord, 0),
	}

	// Add first job
	cc.RecordJob(JobRecord{ID: "first", Type: "test", Status: "success"})

	// Add second job
	cc.RecordJob(JobRecord{ID: "second", Type: "test", Status: "success"})

	// Second job should be at index 0
	if cc.recentJobs[0].ID != "second" {
		t.Errorf("recentJobs[0].ID = %s, want second", cc.recentJobs[0].ID)
	}

	// First job should be at index 1
	if cc.recentJobs[1].ID != "first" {
		t.Errorf("recentJobs[1].ID = %s, want first", cc.recentJobs[1].ID)
	}
}

// TestGetRecentJobs tests that GetRecentJobs returns a copy
func TestGetRecentJobs(t *testing.T) {
	cc := &ControlCenter{
		recentJobs: []JobRecord{
			{ID: "job-1", Type: "test", Status: "success"},
			{ID: "job-2", Type: "test", Status: "failed"},
		},
	}

	jobs := cc.GetRecentJobs()

	// Should have same length
	if len(jobs) != 2 {
		t.Errorf("len(jobs) = %d, want 2", len(jobs))
	}

	// Modifying returned slice should not affect original
	jobs[0].ID = "modified"
	if cc.recentJobs[0].ID != "job-1" {
		t.Errorf("Original was modified: recentJobs[0].ID = %s, want job-1", cc.recentJobs[0].ID)
	}
}

// TestFormatDurationCompact tests the formatDurationCompact function
func TestFormatDurationCompact(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"zero", 0, "-"},
		{"50ms", 50 * time.Millisecond, "50ms"},
		{"500ms", 500 * time.Millisecond, "500ms"},
		{"999ms", 999 * time.Millisecond, "999ms"},
		{"1s", time.Second, "1.0s"},
		{"1.5s", 1500 * time.Millisecond, "1.5s"},
		{"30s", 30 * time.Second, "30.0s"},
		{"59s", 59 * time.Second, "59.0s"},
		{"60s", 60 * time.Second, "1.0m"},
		{"90s", 90 * time.Second, "1.5m"},
		{"5m", 5 * time.Minute, "5.0m"},
		{"10m", 10 * time.Minute, "10.0m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDurationCompact(tt.duration)
			if got != tt.want {
				t.Errorf("formatDurationCompact(%v) = %s, want %s", tt.duration, got, tt.want)
			}
		})
	}
}

// TestFormatDuration tests the formatDuration function (from actions.go)
func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"30s", 30 * time.Second, "30s"},
		{"59s", 59 * time.Second, "59s"},
		{"1m", time.Minute, "1m0s"},
		{"1m30s", 90 * time.Second, "1m30s"},
		{"5m45s", 5*time.Minute + 45*time.Second, "5m45s"},
		{"59m59s", 59*time.Minute + 59*time.Second, "59m59s"},
		{"1h", time.Hour, "1h0m"},
		{"1h30m", 90 * time.Minute, "1h30m"},
		{"2h15m", 2*time.Hour + 15*time.Minute, "2h15m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDuration(tt.duration)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %s, want %s", tt.duration, got, tt.want)
			}
		})
	}
}

// TestDetectSystemTailscale tests the DetectSystemTailscale function with mock scenarios
// Note: This test verifies the parsing logic, not actual tailscale execution
func TestDetectSystemTailscaleURLMatching(t *testing.T) {
	// These tests verify the URL matching logic would work correctly
	// We can't test actual tailscale execution without the binary installed

	tests := []struct {
		name               string
		controlURL         string
		nexusURL           string
		shouldMatchNetwork bool
	}{
		{
			name:               "exact match",
			controlURL:         "https://nexus.aceteam.ai",
			nexusURL:           "https://nexus.aceteam.ai",
			shouldMatchNetwork: true,
		},
		{
			name:               "with trailing slash",
			controlURL:         "https://nexus.aceteam.ai/",
			nexusURL:           "https://nexus.aceteam.ai",
			shouldMatchNetwork: true,
		},
		{
			name:               "different servers",
			controlURL:         "https://login.tailscale.com",
			nexusURL:           "https://nexus.aceteam.ai",
			shouldMatchNetwork: false,
		},
		{
			name:               "localhost test server",
			controlURL:         "http://localhost:8080",
			nexusURL:           "http://localhost:8080",
			shouldMatchNetwork: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the URL matching logic directly
			// This is the same logic used in DetectSystemTailscale
			controlURL := tt.controlURL
			nexusClean := tt.nexusURL

			// Normalize URLs
			controlURL = trimTrailingSlash(controlURL)
			nexusClean = trimTrailingSlash(nexusClean)

			match := controlURL == nexusClean ||
				trimPrefix(controlURL, "https://") == trimPrefix(nexusClean, "https://") ||
				contains(controlURL, trimPrefix(nexusClean, "https://"))

			if match != tt.shouldMatchNetwork {
				t.Errorf("URL match = %v, want %v", match, tt.shouldMatchNetwork)
			}
		})
	}
}

// Helper functions to avoid importing strings package just for tests
func trimTrailingSlash(s string) string {
	if len(s) > 0 && s[len(s)-1] == '/' {
		return s[:len(s)-1]
	}
	return s
}

func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestJobStats tests the JobStats struct
func TestJobStats(t *testing.T) {
	stats := JobStats{
		Pending:    10,
		Processing: 2,
		Completed:  100,
		Failed:     5,
	}

	if stats.Pending != 10 {
		t.Errorf("JobStats.Pending = %d, want 10", stats.Pending)
	}
	if stats.Processing != 2 {
		t.Errorf("JobStats.Processing = %d, want 2", stats.Processing)
	}
	if stats.Completed != 100 {
		t.Errorf("JobStats.Completed = %d, want 100", stats.Completed)
	}
	if stats.Failed != 5 {
		t.Errorf("JobStats.Failed = %d, want 5", stats.Failed)
	}
}

// TestActivityEntry tests the ActivityEntry struct
func TestActivityEntry(t *testing.T) {
	now := time.Now()
	entry := ActivityEntry{
		Time:    now,
		Level:   "success",
		Message: "Job completed",
	}

	if entry.Time != now {
		t.Errorf("ActivityEntry.Time mismatch")
	}
	if entry.Level != "success" {
		t.Errorf("ActivityEntry.Level = %s, want success", entry.Level)
	}
	if entry.Message != "Job completed" {
		t.Errorf("ActivityEntry.Message = %s, want Job completed", entry.Message)
	}
}

// TestServiceInfo tests the ServiceInfo struct
func TestServiceInfo(t *testing.T) {
	svc := ServiceInfo{
		Name:   "vllm",
		Status: "running",
		Uptime: "2h30m",
	}

	if svc.Name != "vllm" {
		t.Errorf("ServiceInfo.Name = %s, want vllm", svc.Name)
	}
	if svc.Status != "running" {
		t.Errorf("ServiceInfo.Status = %s, want running", svc.Status)
	}
	if svc.Uptime != "2h30m" {
		t.Errorf("ServiceInfo.Uptime = %s, want 2h30m", svc.Uptime)
	}
}

// TestUpdateServicesViewRendersFootprint verifies the services panel renders
// the per-service footprint + usage columns and highlights a heavy-and-idle
// service (the #421 eviction candidate) in red.
func TestUpdateServicesViewRendersFootprint(t *testing.T) {
	cc := New(Config{Version: "1.0.0"})
	// updateServicesView reads/writes cc.servicesView; the table is normally
	// created during UI setup, which we don't run headless.
	cc.servicesView = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	cc.data = StatusData{
		ManagedSummary: "managed: RAM 13G/62G · VRAM 21G/24G",
		Services: []ServiceInfo{
			{
				Name:      "vllm",
				Status:    "running",
				Uptime:    "2h",
				Footprint: "RAM 6.1G  VRAM 21.0G  GPU 74%",
				IdleLabel: "busy",
			},
			{
				Name:         "diffusers",
				Status:       "running",
				Uptime:       "1h",
				Footprint:    "RAM 7.4G  VRAM 0.0G  GPU 0%",
				IdleLabel:    "idle 38m",
				HeavyAndIdle: true,
			},
		},
	}

	cc.updateServicesView()

	// Header now has FOOTPRINT and USAGE columns.
	if got := cc.servicesView.GetCell(0, 2).Text; !strings.Contains(got, "FOOTPRINT") {
		t.Errorf("col 2 header = %q, want FOOTPRINT", got)
	}
	if got := cc.servicesView.GetCell(0, 3).Text; !strings.Contains(got, "USAGE") {
		t.Errorf("col 3 header = %q, want USAGE", got)
	}

	// Row 1 = vllm: busy, gray footprint (no red highlight).
	vllmFootprint := cc.servicesView.GetCell(1, 2).Text
	if !strings.Contains(vllmFootprint, "VRAM 21.0G") {
		t.Errorf("vllm footprint cell = %q", vllmFootprint)
	}
	if strings.Contains(vllmFootprint, "red") {
		t.Errorf("busy vllm should not be red-highlighted: %q", vllmFootprint)
	}

	// Row 2 = diffusers: heavy AND idle -> red highlight on both footprint and usage.
	diffFootprint := cc.servicesView.GetCell(2, 2).Text
	if !strings.Contains(diffFootprint, "red") {
		t.Errorf("heavy-and-idle diffusers footprint should be red: %q", diffFootprint)
	}
	diffUsage := cc.servicesView.GetCell(2, 3).Text
	if !strings.Contains(diffUsage, "idle 38m") || !strings.Contains(diffUsage, "red") {
		t.Errorf("heavy-and-idle diffusers usage cell = %q, want red 'idle 38m'", diffUsage)
	}

	// Uptime moved to column 4.
	if got := cc.servicesView.GetCell(1, 4).Text; !strings.Contains(got, "2h") {
		t.Errorf("vllm uptime cell (col 4) = %q, want 2h", got)
	}
}

// TestPeerInfo tests the PeerInfo struct
func TestPeerInfo(t *testing.T) {
	peer := PeerInfo{
		Hostname: "node-2",
		IP:       "100.64.0.2",
		Online:   true,
		Latency:  "5.2ms",
	}

	if peer.Hostname != "node-2" {
		t.Errorf("PeerInfo.Hostname = %s, want node-2", peer.Hostname)
	}
	if peer.IP != "100.64.0.2" {
		t.Errorf("PeerInfo.IP = %s, want 100.64.0.2", peer.IP)
	}
	if !peer.Online {
		t.Error("PeerInfo.Online = false, want true")
	}
	if peer.Latency != "5.2ms" {
		t.Errorf("PeerInfo.Latency = %s, want 5.2ms", peer.Latency)
	}
}

// TestPortForward tests the PortForward struct
func TestPortForward(t *testing.T) {
	now := time.Now()
	fwd := PortForward{
		LocalPort:   8080,
		Description: "web server",
		StartedAt:   now,
	}

	if fwd.LocalPort != 8080 {
		t.Errorf("PortForward.LocalPort = %d, want 8080", fwd.LocalPort)
	}
	if fwd.Description != "web server" {
		t.Errorf("PortForward.Description = %s, want web server", fwd.Description)
	}
	if fwd.StartedAt != now {
		t.Errorf("PortForward.StartedAt mismatch")
	}
}

// TestNewControlCenter tests the New function creates a properly initialized ControlCenter
func TestNewControlCenter(t *testing.T) {
	cfg := Config{
		Version:        "1.0.0",
		AuthServiceURL: "https://aceteam.ai",
		NexusURL:       "https://nexus.aceteam.ai",
	}

	cc := New(cfg)

	if cc == nil {
		t.Fatal("New returned nil")
	}
	if cc.data.Version != "1.0.0" {
		t.Errorf("data.Version = %s, want 1.0.0", cc.data.Version)
	}
	if cc.authServiceURL != "https://aceteam.ai" {
		t.Errorf("authServiceURL = %s, want https://aceteam.ai", cc.authServiceURL)
	}
	if cc.nexusURL != "https://nexus.aceteam.ai" {
		t.Errorf("nexusURL = %s, want https://nexus.aceteam.ai", cc.nexusURL)
	}
	if cc.stopChan == nil {
		t.Error("stopChan should be initialized")
	}
	if cc.activities == nil {
		t.Error("activities should be initialized")
	}
	if cc.activeForwards == nil {
		t.Error("activeForwards should be initialized")
	}
}

// TestControlCenterWithCallbacks tests that callbacks are properly set
func TestControlCenterWithCallbacks(t *testing.T) {
	refreshCalled := false
	startServiceCalled := false
	stopServiceCalled := false

	cfg := Config{
		Version: "1.0.0",
		RefreshFn: func() (StatusData, error) {
			refreshCalled = true
			return StatusData{}, nil
		},
		StartServiceFn: func(name string) error {
			startServiceCalled = true
			return nil
		},
		StopServiceFn: func(name string) error {
			stopServiceCalled = true
			return nil
		},
	}

	cc := New(cfg)

	// Test refresh callback
	if cc.refreshFn != nil {
		_, _ = cc.refreshFn()
		if !refreshCalled {
			t.Error("refreshFn callback was not called")
		}
	}

	// Test start service callback
	if cc.startServiceFn != nil {
		_ = cc.startServiceFn("test")
		if !startServiceCalled {
			t.Error("startServiceFn callback was not called")
		}
	}

	// Test stop service callback
	if cc.stopServiceFn != nil {
		_ = cc.stopServiceFn("test")
		if !stopServiceCalled {
			t.Error("stopServiceFn callback was not called")
		}
	}
}

// TestStatusDataDualConnectionFields tests the dual connection detection fields
func TestStatusDataDualConnectionFields(t *testing.T) {
	data := StatusData{
		Connected:              true,
		NodeIP:                 "100.64.0.1",
		SystemTailscaleRunning: true,
		SystemTailscaleIP:      "100.64.0.2",
		SystemTailscaleName:    "my-machine",
		DualConnection:         true,
	}

	if !data.Connected {
		t.Error("StatusData.Connected = false, want true")
	}
	if !data.SystemTailscaleRunning {
		t.Error("StatusData.SystemTailscaleRunning = false, want true")
	}
	if data.SystemTailscaleIP != "100.64.0.2" {
		t.Errorf("StatusData.SystemTailscaleIP = %s, want 100.64.0.2", data.SystemTailscaleIP)
	}
	if data.SystemTailscaleName != "my-machine" {
		t.Errorf("StatusData.SystemTailscaleName = %s, want my-machine", data.SystemTailscaleName)
	}
	if !data.DualConnection {
		t.Error("StatusData.DualConnection = false, want true")
	}
}

// TestRefreshInterval tests the constant value
func TestRefreshInterval(t *testing.T) {
	expected := 30 * time.Second
	if RefreshInterval != expected {
		t.Errorf("RefreshInterval = %v, want %v", RefreshInterval, expected)
	}
}

// TestDeviceAuthConfig tests the DeviceAuthConfig struct
func TestDeviceAuthConfig(t *testing.T) {
	cfg := DeviceAuthConfig{
		UserCode:        "ABCD-1234",
		VerificationURI: "https://aceteam.ai/device",
		DeviceCode:      "device-code-xyz",
		ExpiresIn:       900,
		Interval:        5,
	}

	if cfg.UserCode != "ABCD-1234" {
		t.Errorf("DeviceAuthConfig.UserCode = %s, want ABCD-1234", cfg.UserCode)
	}
	if cfg.VerificationURI != "https://aceteam.ai/device" {
		t.Errorf("DeviceAuthConfig.VerificationURI = %s, want https://aceteam.ai/device", cfg.VerificationURI)
	}
	if cfg.ExpiresIn != 900 {
		t.Errorf("DeviceAuthConfig.ExpiresIn = %d, want 900", cfg.ExpiresIn)
	}
	if cfg.Interval != 5 {
		t.Errorf("DeviceAuthConfig.Interval = %d, want 5", cfg.Interval)
	}
}

// TestDeviceAuthModalContentIncludesEnrollPayload asserts the device-auth modal
// body shows the exact enrollment payload (verification_uri_complete + &v=1) —
// the same string encoded into the scannable QR — so the URL, copy-link, and QR
// all point to an identical target. Mirrors qrcode_test.go's payload assertion.
func TestDeviceAuthModalContentIncludesEnrollPayload(t *testing.T) {
	cfg := &DeviceAuthConfig{
		UserCode:        "ABCD-1234",
		VerificationURI: "https://aceteam.ai/device",
		DeviceCode:      "device-secret-should-never-appear",
		ExpiresIn:       900,
		Interval:        5,
	}
	payload := ui.BuildEnrollPayload(cfg.VerificationURI, cfg.UserCode)
	if payload != "https://aceteam.ai/device?code=ABCD-1234&v=1" {
		t.Fatalf("unexpected enroll payload: %q", payload)
	}

	content := buildDeviceAuthContent(cfg, payload, "waiting", time.Now().Add(15*time.Minute))

	if !strings.Contains(content, payload) {
		t.Errorf("modal content missing enroll payload %q:\n%s", payload, content)
	}
	if !strings.Contains(content, cfg.UserCode) {
		t.Errorf("modal content missing manual user code %q", cfg.UserCode)
	}
	// The hotkey row must be preserved.
	if !strings.Contains(content, "open browser") || !strings.Contains(content, "copy link") {
		t.Errorf("modal content missing hotkey row:\n%s", content)
	}
	// Security guardrail: the device_code polling secret must never be shown.
	if strings.Contains(content, cfg.DeviceCode) {
		t.Errorf("modal content leaked device_code secret")
	}
}

// TestDeviceAuthQRMatchesPayload asserts the modal's QR renders the exact
// enrollment payload via the tview-safe full-block renderer.
func TestDeviceAuthQRMatchesPayload(t *testing.T) {
	cfg := &DeviceAuthConfig{UserCode: "WXYZ-5678", VerificationURI: "https://aceteam.ai/device"}

	qr := ui.RenderEnrollQRBlocks(cfg.VerificationURI, cfg.UserCode)
	if strings.TrimSpace(qr) == "" {
		t.Fatal("device-auth QR is empty")
	}
	want := ui.RenderQRCodeBlocks(ui.BuildEnrollPayload(cfg.VerificationURI, cfg.UserCode))
	if qr != want {
		t.Error("device-auth QR does not match the exact BuildEnrollPayload rendering")
	}
	// tview-safe: no ANSI escapes, full blocks present (see ui.RenderQRCodeBlocks).
	if strings.Contains(qr, "\x1b[") {
		t.Error("device-auth QR leaked ANSI escapes (would corrupt in tview)")
	}
	if !strings.Contains(qr, "██") {
		t.Error("device-auth QR missing full-block glyphs")
	}
}

// TestVisibleIndices tests that visibleIndices returns only visible page indices
func TestVisibleIndices(t *testing.T) {
	pm := &PageManager{}
	pm.registered = []registeredPage{
		{page: NewPlaceholderPage("dashboard", "Dashboard"), visible: true},
		{page: NewPlaceholderPage("console", "Console"), visible: false},
		{page: NewPlaceholderPage("services", "Services"), visible: false},
		{page: NewPlaceholderPage("jobs", "Jobs"), visible: false},
		{page: NewPlaceholderPage("network", "Network"), visible: false},
	}

	vis := pm.visibleIndices()
	if len(vis) != 1 {
		t.Fatalf("visibleIndices() returned %d indices, want 1", len(vis))
	}
	if vis[0] != 0 {
		t.Errorf("visibleIndices()[0] = %d, want 0", vis[0])
	}
}

// TestShowHide tests Show and Hide methods
func TestShowHide(t *testing.T) {
	pm := &PageManager{
		tabBar: tview.NewTextView().SetDynamicColors(true),
	}
	pm.registered = []registeredPage{
		{page: NewPlaceholderPage("dashboard", "Dashboard"), visible: true},
		{page: NewPlaceholderPage("console", "Console"), visible: false},
		{page: NewPlaceholderPage("services", "Services"), visible: false},
	}

	// Show services
	pm.Show("services")
	vis := pm.visibleIndices()
	if len(vis) != 2 {
		t.Fatalf("after Show('services'), visibleIndices() = %d, want 2", len(vis))
	}
	if vis[1] != 2 {
		t.Errorf("visibleIndices()[1] = %d, want 2", vis[1])
	}

	// Hide it back
	pm.Hide("services")
	vis = pm.visibleIndices()
	if len(vis) != 1 {
		t.Fatalf("after Hide('services'), visibleIndices() = %d, want 1", len(vis))
	}
}

// TestSwitchToName tests switching to a page by name
func TestSwitchToName(t *testing.T) {
	pm := &PageManager{
		pages:  tview.NewPages(),
		tabBar: tview.NewTextView().SetDynamicColors(true),
	}

	// Build placeholder pages manually
	p1 := NewPlaceholderPage("dashboard", "Dashboard")
	p2 := NewPlaceholderPage("services", "Services")
	p1.Build(nil)
	p2.Build(nil)

	pm.registered = []registeredPage{
		{page: p1, visible: true},
		{page: p2, visible: true},
	}
	pm.pages.AddPage("dashboard", p1.view, true, false)
	pm.pages.AddPage("services", p2.view, true, false)
	pm.activeIdx = 0

	pm.SwitchToName("services")
	if pm.activeIdx != 1 {
		t.Errorf("after SwitchToName('services'), activeIdx = %d, want 1", pm.activeIdx)
	}
}

// TestDenseAltMapping tests that Alt+N maps to the Nth visible page
func TestDenseAltMapping(t *testing.T) {
	pm := &PageManager{}
	pm.registered = []registeredPage{
		{page: NewPlaceholderPage("dashboard", "Dashboard"), visible: true},
		{page: NewPlaceholderPage("console", "Console"), visible: false},
		{page: NewPlaceholderPage("services", "Services"), visible: true},
		{page: NewPlaceholderPage("jobs", "Jobs"), visible: false},
		{page: NewPlaceholderPage("network", "Network"), visible: true},
	}

	vis := pm.visibleIndices()
	if len(vis) != 3 {
		t.Fatalf("visibleIndices() returned %d, want 3", len(vis))
	}
	// Alt+1 -> index 0 (dashboard)
	if vis[0] != 0 {
		t.Errorf("Alt+1 maps to index %d, want 0", vis[0])
	}
	// Alt+2 -> index 2 (services, skipping hidden console)
	if vis[1] != 2 {
		t.Errorf("Alt+2 maps to index %d, want 2", vis[1])
	}
	// Alt+3 -> index 4 (network, skipping hidden jobs)
	if vis[2] != 4 {
		t.Errorf("Alt+3 maps to index %d, want 4", vis[2])
	}
}

// TestNextVisibleIndex covers the Tab-forward tab-cycle order used by the shared
// navigation convention: skip hidden pages, wrap at the end, and no-op when a
// single or no page is visible.
func TestNextVisibleIndex(t *testing.T) {
	cases := []struct {
		name     string
		visibles []int
		cur      int
		want     int
	}{
		{"advance to next visible", []int{0, 2, 4}, 0, 2},
		{"skip hidden between", []int{0, 2, 4}, 2, 4},
		{"wrap to first", []int{0, 2, 4}, 4, 0},
		{"cur not in set advances to next greater", []int{0, 2, 4}, 1, 2},
		{"cur beyond last wraps", []int{0, 2, 4}, 5, 0},
		{"single visible stays", []int{3}, 3, 3},
		{"empty stays", nil, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextVisibleIndex(tc.visibles, tc.cur); got != tc.want {
				t.Errorf("nextVisibleIndex(%v, %d) = %d, want %d", tc.visibles, tc.cur, got, tc.want)
			}
		})
	}
}

// TestPrevVisibleIndex covers the Shift+Tab-backward tab-cycle order: skip hidden
// pages, wrap at the start, and no-op for single/empty sets.
func TestPrevVisibleIndex(t *testing.T) {
	cases := []struct {
		name     string
		visibles []int
		cur      int
		want     int
	}{
		{"back to previous visible", []int{0, 2, 4}, 4, 2},
		{"skip hidden between", []int{0, 2, 4}, 2, 0},
		{"wrap to last", []int{0, 2, 4}, 0, 4},
		{"cur not in set goes to next lower", []int{0, 2, 4}, 3, 2},
		{"cur below first wraps to last", []int{0, 2, 4}, -1, 4},
		{"single visible stays", []int{3}, 3, 3},
		{"empty stays", nil, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := prevVisibleIndex(tc.visibles, tc.cur); got != tc.want {
				t.Errorf("prevVisibleIndex(%v, %d) = %d, want %d", tc.visibles, tc.cur, got, tc.want)
			}
		})
	}
}

// TestVisibleCycleRoundTrip asserts nextVisibleIndex over all visible pages
// returns to the start, i.e. Tab cycles through every visible tab exactly once.
func TestVisibleCycleRoundTrip(t *testing.T) {
	visibles := []int{0, 1, 3, 6}
	cur := visibles[0]
	seen := map[int]bool{cur: true}
	for i := 0; i < len(visibles)-1; i++ {
		cur = nextVisibleIndex(visibles, cur)
		if seen[cur] {
			t.Fatalf("cycle revisited index %d before covering all visibles", cur)
		}
		seen[cur] = true
	}
	// One more Tab wraps back to the first visible page.
	if got := nextVisibleIndex(visibles, cur); got != visibles[0] {
		t.Errorf("cycle did not wrap to first: got %d, want %d", got, visibles[0])
	}
	if len(seen) != len(visibles) {
		t.Errorf("cycle covered %d of %d visible tabs", len(seen), len(visibles))
	}
}

// TestProxmoxTabVisible asserts the Proxmox tab is gated on real detection: shown
// only when Enabled AND a BaseURL is set (never folded into the module list).
func TestProxmoxTabVisible(t *testing.T) {
	cases := []struct {
		name string
		cfg  ProxmoxConfig
		want bool
	}{
		{"disabled (no detection)", ProxmoxConfig{}, false},
		{"detected with URL", ProxmoxConfig{Enabled: true, BaseURL: "https://localhost:8006"}, true},
		{"enabled but no URL (defensive)", ProxmoxConfig{Enabled: true}, false},
		{"URL but not enabled", ProxmoxConfig{BaseURL: "https://localhost:8006"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxmoxTabVisible(tc.cfg); got != tc.want {
				t.Errorf("proxmoxTabVisible(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}
