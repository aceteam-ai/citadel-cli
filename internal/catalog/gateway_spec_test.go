package catalog

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestGatewayBlockParses: a manifest with a gateway: block unmarshals into the
// GatewaySpec fields.
func TestGatewayBlockParses(t *testing.T) {
	src := `
name: whatsapp-bridge
version: 1.0.0
gateway:
  prefix: whatsapp
  port_env: BRIDGE_PORT
  capability: provision
`
	var m ServiceManifest
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Gateway == nil {
		t.Fatal("Gateway is nil, want parsed block")
	}
	if m.Gateway.Prefix != "whatsapp" {
		t.Errorf("prefix = %q, want whatsapp", m.Gateway.Prefix)
	}
	if m.Gateway.PortEnv != "BRIDGE_PORT" {
		t.Errorf("port_env = %q, want BRIDGE_PORT", m.Gateway.PortEnv)
	}
	if m.Gateway.Capability != "provision" {
		t.Errorf("capability = %q, want provision", m.Gateway.Capability)
	}
	if err := m.Gateway.Validate(); err != nil {
		t.Errorf("Validate on a good block: %v", err)
	}
}

// TestGatewayBlockAbsent: no gateway: block leaves Gateway nil (module not
// exposed), and Validate on nil is fine.
func TestGatewayBlockAbsent(t *testing.T) {
	var m ServiceManifest
	if err := yaml.Unmarshal([]byte("name: plain\nversion: 1.0.0\n"), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Gateway != nil {
		t.Errorf("Gateway = %+v, want nil for a manifest with no gateway block", m.Gateway)
	}
	if err := m.Gateway.Validate(); err != nil {
		t.Errorf("Validate on nil gateway: %v", err)
	}
}

// TestGatewayCapabilityDefaults: an omitted capability yields the provision
// default; a present one is honored.
func TestGatewayCapabilityDefaults(t *testing.T) {
	g := &GatewaySpec{Prefix: "svc", PortEnv: "PORT"}
	if err := g.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := g.EffectiveCapability(); got != DefaultGatewayCapability {
		t.Errorf("EffectiveCapability = %q, want %q", got, DefaultGatewayCapability)
	}
	g2 := &GatewaySpec{Prefix: "svc", Capability: "services"}
	if got := g2.EffectiveCapability(); got != "services" {
		t.Errorf("EffectiveCapability = %q, want services", got)
	}
}

// TestGatewayPrefixValidation: slashes, "..", empty, uppercase, and unknown
// capabilities are all rejected; clean slugs pass.
func TestGatewayPrefixValidation(t *testing.T) {
	bad := []struct {
		name string
		spec GatewaySpec
		want string // substring the error should mention
	}{
		{"slash", GatewaySpec{Prefix: "a/b"}, "slashes"},
		{"backslash", GatewaySpec{Prefix: "a\\b"}, "slashes"},
		{"traversal", GatewaySpec{Prefix: ".."}, "'..'"},
		{"embedded traversal", GatewaySpec{Prefix: "a..b"}, "'..'"},
		{"empty", GatewaySpec{Prefix: ""}, "required"},
		{"uppercase", GatewaySpec{Prefix: "WhatsApp"}, "lowercase"},
		{"space", GatewaySpec{Prefix: "a b"}, "lowercase"},
		{"unknown capability", GatewaySpec{Prefix: "svc", Capability: "root"}, "not a known permission"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want error", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}

	good := []string{"whatsapp", "my-module", "svc1", "a-b-c", "x"}
	for _, p := range good {
		t.Run("ok:"+p, func(t *testing.T) {
			g := GatewaySpec{Prefix: p}
			if err := g.Validate(); err != nil {
				t.Errorf("Validate(%q) = %v, want nil", p, err)
			}
		})
	}
}
