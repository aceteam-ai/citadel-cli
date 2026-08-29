package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registeredOnlineIdentity builds a NodeIdentity shaped like a fully
// registered, online node -- mirroring what gatherIdentity would produce
// with a manifest, device config, and a live network connection.
func registeredOnlineIdentity() NodeIdentity {
	return NodeIdentity{
		NodeName:           "my-3090-box",
		Hostname:           "my-3090-box",
		HeadscaleNodeID:    "42",
		Connected:          true,
		MeshIPv4:           "100.64.0.5",
		OrgID:              "org-123",
		OrgName:            "Acme Inc",
		UserEmail:          "jason@example.com",
		UserName:           "Jason",
		Registered:         true,
		CitadelVersion:     "v2.116.0",
		HeartbeatFreshness: "ok",
		LastHeartbeatAt:    "2026-08-25T00:00:00Z",
		NodeConfigDir:      "/home/jason/citadel-node",
	}
}

// unregisteredIdentity builds a NodeIdentity shaped like a fresh host that has
// never run `citadel init`/`citadel login` -- what gatherIdentity produces
// when every persisted source is absent.
func unregisteredIdentity() NodeIdentity {
	return NodeIdentity{
		Hostname:           "fresh-host",
		Registered:         false,
		CitadelVersion:     "v2.116.0",
		HeartbeatFreshness: "unknown",
		NodeConfigDir:      "/home/user/citadel-node",
		Warnings: []string{
			"citadel.yaml not found: global config not found",
			"not connected to AceTeam Network (run 'citadel init' or 'citadel login')",
			"this host does not appear to be a registered citadel node (no citadel.yaml, device config, or network state found)",
		},
	}
}

func TestRenderIdentity_RegisteredOnline(t *testing.T) {
	var buf bytes.Buffer
	renderIdentity(&buf, registeredOnlineIdentity())
	out := buf.String()

	for _, want := range []string{
		"Citadel Node Identity",
		"my-3090-box",
		"Headscale node ID:",
		"42",
		"online",
		"100.64.0.5",
		"Acme Inc",
		"org-123",
		"jason@example.com",
		"ok",
		"registered citadel node",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report missing %q\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT a registered") {
		t.Errorf("registered node should not print the NOT-registered line:\n%s", out)
	}
}

// TestRenderIdentity_UnregisteredDegradesGracefully pins the guardrail: an
// unregistered/offline host must still render a well-formed, non-panicking
// report with unavailable fields clearly marked "unknown", not fabricated.
func TestRenderIdentity_UnregisteredDegradesGracefully(t *testing.T) {
	var buf bytes.Buffer
	renderIdentity(&buf, unregisteredIdentity())
	out := buf.String()

	for _, want := range []string{
		"Citadel Node Identity",
		"unknown",
		"offline",
		"not available locally",
		"WARNINGS",
		"not connected to AceTeam Network",
		"NOT a registered citadel node",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report missing %q\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Overall: this host is a registered") {
		t.Errorf("unregistered node should not claim it is registered:\n%s", out)
	}
}

func TestRenderIdentity_PlatformNodeIDShownWhenPresent(t *testing.T) {
	id := registeredOnlineIdentity()
	id.PlatformNodeID = "1297"

	var buf bytes.Buffer
	renderIdentity(&buf, id)
	out := buf.String()

	if !strings.Contains(out, "1297") {
		t.Errorf("expected platform node ID to be rendered when present:\n%s", out)
	}
	if strings.Contains(out, "not available locally") {
		t.Errorf("should not print the unavailable note when PlatformNodeID is set:\n%s", out)
	}
}

// TestResolvePlatformNodeID pins the aceteam #8139 preference order: the
// device-config FabricNodeID wins whenever present, and the legacy
// SSHSyncConfig fallback is used only when it's empty.
func TestResolvePlatformNodeID(t *testing.T) {
	cases := []struct {
		name         string
		fabricNodeID string
		sshSyncID    string
		want         string
	}{
		{"fabric node id present, wins over ssh sync fallback", "1297", "999", "1297"},
		{"fabric node id absent, falls back to ssh sync", "", "999", "999"},
		{"both absent", "", "", ""},
		{"fabric node id present, ssh sync absent", "1297", "", "1297"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePlatformNodeID(tc.fabricNodeID, tc.sshSyncID); got != tc.want {
				t.Errorf("resolvePlatformNodeID(%q, %q) = %q, want %q", tc.fabricNodeID, tc.sshSyncID, got, tc.want)
			}
		})
	}
}

func TestOrgDisplayString(t *testing.T) {
	cases := []struct {
		name string
		id   NodeIdentity
		want string
	}{
		{"both", NodeIdentity{OrgName: "Acme", OrgID: "org-1"}, "Acme (org-1)"},
		{"name only", NodeIdentity{OrgName: "Acme"}, "Acme"},
		{"id only", NodeIdentity{OrgID: "org-1"}, "org-1"},
		{"neither", NodeIdentity{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := orgDisplayString(c.id); got != c.want {
				t.Errorf("orgDisplayString() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestUserDisplayString(t *testing.T) {
	cases := []struct {
		name string
		id   NodeIdentity
		want string
	}{
		{"both", NodeIdentity{UserName: "Jason", UserEmail: "j@x.com"}, "Jason <j@x.com>"},
		{"email only", NodeIdentity{UserEmail: "j@x.com"}, "j@x.com"},
		{"name only", NodeIdentity{UserName: "Jason"}, "Jason"},
		{"neither", NodeIdentity{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := userDisplayString(c.id); got != c.want {
				t.Errorf("userDisplayString() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestWhoamiJSON_EmitsExpectedFields exercises the exact JSON-encode path
// whoamiCmd's RunE uses (json.NewEncoder with 2-space indent) against an
// injected NodeIdentity, and asserts the fields the aceteam #8139 ask calls
// out by name: fabric/mesh node ID, online status, org, and citadel version.
func TestWhoamiJSON_EmitsExpectedFields(t *testing.T) {
	id := registeredOnlineIdentity()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(id); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, field := range []string{
		"node_name", "headscale_node_id", "connected", "org_id", "org_name",
		"citadel_version", "registered", "heartbeat_freshness",
	} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("expected JSON output to include field %q, got keys: %v", field, decoded)
		}
	}
	if decoded["connected"] != true {
		t.Errorf("expected connected=true, got %v", decoded["connected"])
	}
	if decoded["citadel_version"] != "v2.116.0" {
		t.Errorf("expected citadel_version echoed, got %v", decoded["citadel_version"])
	}
	// PlatformNodeID/HeadscaleNodeID: fabric numeric ID is NOT available
	// locally (the #8139 finding) -- headscale_node_id must be present,
	// platform_node_id must be absent (omitempty, unset in this fixture).
	if _, ok := decoded["platform_node_id"]; ok {
		t.Errorf("expected platform_node_id to be omitted when empty, got %v", decoded["platform_node_id"])
	}
}

// TestWhoamiJSON_UnregisteredOmitsUnknownFields pins that an unregistered
// node's JSON output omits (rather than fabricates) unavailable identity
// fields, while still reporting registered=false and warnings.
func TestWhoamiJSON_UnregisteredOmitsUnknownFields(t *testing.T) {
	id := unregisteredIdentity()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(id); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded["registered"] != false {
		t.Errorf("expected registered=false, got %v", decoded["registered"])
	}
	for _, absent := range []string{"headscale_node_id", "platform_node_id", "org_id", "mesh_ipv4"} {
		if v, ok := decoded[absent]; ok {
			t.Errorf("expected %q to be omitted (unknown, not fabricated) for an unregistered node, got %v", absent, v)
		}
	}
	warnings, ok := decoded["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Errorf("expected warnings to be populated for an unregistered node, got %v", decoded["warnings"])
	}
}

func TestWriteIdentityCache_WritesJSONWith0600(t *testing.T) {
	dir := t.TempDir()
	nodeConfigDir := filepath.Join(dir, "citadel-node")

	id := registeredOnlineIdentity()
	id.NodeConfigDir = nodeConfigDir

	if err := writeIdentityCache(nodeConfigDir, id); err != nil {
		t.Fatalf("writeIdentityCache: %v", err)
	}

	path := filepath.Join(nodeConfigDir, identityCacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected identity cache file to exist: %v", err)
	}

	var decoded NodeIdentity
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("expected valid JSON, got error: %v\ncontents: %s", err, data)
	}
	if decoded.NodeName != id.NodeName {
		t.Errorf("expected NodeName %q, got %q", id.NodeName, decoded.NodeName)
	}
	if decoded.HeadscaleNodeID != id.HeadscaleNodeID {
		t.Errorf("expected HeadscaleNodeID %q, got %q", id.HeadscaleNodeID, decoded.HeadscaleNodeID)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected identity.json to be mode 0600, got %o", perm)
	}
}

// TestWriteIdentityCache_IdempotentRefresh pins that a second call
// overwrites (not appends to) the cache with the latest gather.
func TestWriteIdentityCache_IdempotentRefresh(t *testing.T) {
	dir := t.TempDir()
	nodeConfigDir := filepath.Join(dir, "citadel-node")

	first := registeredOnlineIdentity()
	first.NodeConfigDir = nodeConfigDir
	if err := writeIdentityCache(nodeConfigDir, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	second := first
	second.NodeName = "renamed-box"
	second.HeadscaleNodeID = "99"
	if err := writeIdentityCache(nodeConfigDir, second); err != nil {
		t.Fatalf("second write: %v", err)
	}

	path := filepath.Join(nodeConfigDir, identityCacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded NodeIdentity
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.NodeName != "renamed-box" {
		t.Errorf("expected cache to reflect latest gather (renamed-box), got %q", decoded.NodeName)
	}
	if decoded.HeadscaleNodeID != "99" {
		t.Errorf("expected cache to reflect latest gather (99), got %q", decoded.HeadscaleNodeID)
	}
}

func TestWriteIdentityCache_EmptyDirErrors(t *testing.T) {
	if err := writeIdentityCache("", registeredOnlineIdentity()); err == nil {
		t.Fatalf("expected an error when nodeConfigDir is empty")
	}
}

// TestGatherIdentityNoCrash exercises the real wiring end-to-end (manifest
// lookup, device config, network state, heartbeat marker) the way
// TestRunDoctorChecksNoCrash does for `citadel doctor`: it intentionally does
// not assert on registration state, since that depends on the machine
// running the test, only that gathering never panics and always returns a
// well-formed, renderable NodeIdentity.
//
// Global-state note: on a machine with real saved tsnet state (HasState()
// true), gatherIdentity's VerifyOrReconnect call can succeed and call
// network.SetGlobal, leaving a live server in internal/network's package
// global for the rest of this test binary's run. Harmless in CI (no state ⇒
// HasState() false ⇒ this whole branch is skipped) and this repo's existing
// network tests already assume a mutable global singleton, but worth stating
// explicitly here rather than leaving the next flaky-test hunt to
// rediscover it.
func TestGatherIdentityNoCrash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*identityNetworkTimeout)
	defer cancel()

	id := gatherIdentity(ctx)

	if id.CitadelVersion == "" {
		t.Errorf("expected CitadelVersion to always be set")
	}
	if id.NodeConfigDir == "" {
		t.Errorf("expected NodeConfigDir to always be resolved")
	}
	if id.HeartbeatFreshness == "" {
		t.Errorf("expected HeartbeatFreshness to always be set (at least \"unknown\")")
	}

	var buf bytes.Buffer
	renderIdentity(&buf, id) // must not panic regardless of local state
	if buf.Len() == 0 {
		t.Fatalf("expected renderIdentity to write output")
	}
}
