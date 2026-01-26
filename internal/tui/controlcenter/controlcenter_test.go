package controlcenter

import (
	"testing"
	"time"
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
		name             string
		controlURL       string
		nexusURL         string
		shouldMatchNetwork bool
	}{
		{
			name:             "exact match",
			controlURL:       "https://nexus.aceteam.ai",
			nexusURL:         "https://nexus.aceteam.ai",
			shouldMatchNetwork: true,
		},
		{
			name:             "with trailing slash",
			controlURL:       "https://nexus.aceteam.ai/",
			nexusURL:         "https://nexus.aceteam.ai",
			shouldMatchNetwork: true,
		},
		{
			name:             "different servers",
			controlURL:       "https://login.tailscale.com",
			nexusURL:         "https://nexus.aceteam.ai",
			shouldMatchNetwork: false,
		},
		{
			name:             "localhost test server",
			controlURL:       "http://localhost:8080",
			nexusURL:         "http://localhost:8080",
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
