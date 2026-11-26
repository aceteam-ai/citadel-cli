package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestServerSettingPrecedenceAndAliases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input serverSelection
		want  string
	}{
		{"default", serverSelection{}, "https://aceteam.ai"},
		{"saved", serverSelection{Saved: "https://saved.example.test"}, "https://saved.example.test"},
		{"environment", serverSelection{Env: "env.example.test"}, "https://env.example.test"},
		{"legacy environment", serverSelection{LegacyEnv: "legacy.example.test"}, "https://legacy.example.test"},
		{"new environment wins", serverSelection{Env: "env.example.test", LegacyEnv: "legacy.example.test"}, "https://env.example.test"},
		{"flag wins", serverSelection{Flag: "flag.example.test", FlagSet: true, Env: "env.example.test"}, "https://flag.example.test"},
		{"alias wins environment", serverSelection{LegacyFlag: "alias.example.test", LegacyFlagSet: true, Env: "env.example.test"}, "https://alias.example.test"},
		{"equivalent aliases", serverSelection{Flag: "SAVED.EXAMPLE.TEST:443/", FlagSet: true, LegacyFlag: "https://saved.example.test", LegacyFlagSet: true, Saved: "https://saved.example.test"}, "https://saved.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveServerSelection(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("got %q %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestServerSettingCanonicalOrigins(t *testing.T) {
	for input, want := range map[string]string{
		" HTTPS://SERVER.EXAMPLE.TEST.:443/ ": "https://server.example.test",
		"server.example.test:8443":            "https://server.example.test:8443",
		"https://[2001:db8::1]:443":           "https://[2001:db8::1]",
		"http://localhost:8080":               "http://localhost:8080",
		"http://[::1]:80/":                    "http://[::1]",
	} {
		got, err := normalizeServerURL(input)
		if err != nil || got != want {
			t.Errorf("normalize %q: %q %v, want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"https://host.test#", "https://host.test?", "https://host.test:0", "https://host.test:65536", "https://host.test:", "https://bad_host.test", "http://192.0.2.1", "https://host.test/a", "https://host.test/%2F"} {
		if _, err := normalizeServerURL(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestServerSettingPreparedForCommandConsumers(t *testing.T) {
	oldDir, oldConfig := nodeConfigDirFn, deviceConfigForCATrustFn
	oldAuth, oldNexus, oldCA := authServiceURL, nexusURL, caCertPath
	t.Cleanup(func() {
		nodeConfigDirFn, deviceConfigForCATrustFn = oldDir, oldConfig
		authServiceURL, nexusURL, caCertPath = oldAuth, oldNexus, oldCA
	})
	t.Setenv("CITADEL_SERVER", "")
	t.Setenv("CITADEL_AUTH_HOST", "")
	dir := t.TempDir()
	nodeConfigDirFn = func() string { return dir }
	caCertPath = ""
	for _, tc := range []struct {
		name, saved, flag, env, alias string
		refuse                        bool
	}{
		{name: "saved reconnect", saved: "https://saved.example.test/"},
		{name: "visible flag", flag: "saved.example.test"},
		{name: "environment", env: "saved.example.test"},
		{name: "compatibility flag", alias: "saved.example.test"},
		{name: "enrolled switch", saved: "https://saved.example.test", flag: "other.example.test", refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CITADEL_SERVER", tc.env)
			deviceConfigForCATrustFn = func() *DeviceConfig { return &DeviceConfig{APIBaseURL: tc.saved} }
			authServiceURL, nexusURL = defaultControlPlaneServer, "https://coordination.example.test"
			cmd := &cobra.Command{}
			cmd.Flags().String("server", "", "")
			cmd.Flags().String("auth-service", "", "")
			cmd.Flags().String("nexus", "", "")
			if err := cmd.Flags().Set("nexus", nexusURL); err != nil {
				t.Fatal(err)
			}
			if tc.flag != "" {
				if err := cmd.Flags().Set("server", tc.flag); err != nil {
					t.Fatal(err)
				}
			}
			if tc.alias != "" {
				if err := cmd.Flags().Set("auth-service", tc.alias); err != nil {
					t.Fatal(err)
				}
			}
			err := preparePrivateCATrust(cmd)
			if tc.refuse {
				if err == nil || authServiceURL != defaultControlPlaneServer {
					t.Fatalf("switch was not refused before changing consumers: %v %q", err, authServiceURL)
				}
				return
			}
			if err != nil || authServiceURL != "https://saved.example.test" || nexusURL != "https://coordination.example.test" {
				t.Fatalf("consumer endpoints: %q %q %v", authServiceURL, nexusURL, err)
			}
		})
	}
}

func TestServerSettingVisibleFlagAndIdentityOutput(t *testing.T) {
	flag := rootCmd.PersistentFlags().Lookup("server")
	if flag == nil || flag.Hidden || !strings.Contains(flag.Usage, "CITADEL_SERVER") {
		t.Fatal("server flag is not discoverable")
	}
	if alias := rootCmd.PersistentFlags().Lookup("auth-service"); alias == nil || !alias.Hidden {
		t.Fatal("legacy flag missing or no longer hidden")
	}
	id := NodeIdentity{Server: "https://saved.example.test"}
	var output bytes.Buffer
	renderIdentity(&output, id)
	if !strings.Contains(output.String(), "Server:") || !strings.Contains(output.String(), id.Server) {
		t.Fatal("identity output omits server")
	}
	encoded, err := json.Marshal(id)
	if err != nil || !bytes.Contains(encoded, []byte(`"server":"https://saved.example.test"`)) {
		t.Fatalf("identity JSON: %s %v", encoded, err)
	}
}

func TestServerSettingRefusesDifferentSavedServerAndUnsafeInput(t *testing.T) {
	for _, input := range []serverSelection{
		{Flag: "other.example.test", FlagSet: true, Saved: "https://saved.example.test"},
		{Env: "other.example.test", Saved: "https://saved.example.test"},
		{Flag: "one.example.test", FlagSet: true, LegacyFlag: "two.example.test", LegacyFlagSet: true},
		{Flag: "", FlagSet: true}, {Flag: "https://user:fixture-secret@example.test", FlagSet: true},
		{Flag: "https://example.test/path", FlagSet: true}, {Flag: "https://example.test?key=fixture-secret", FlagSet: true},
		{Flag: "file:///fixture-secret", FlagSet: true}, {Flag: "http://example.test", FlagSet: true},
		{Saved: "invalid\nfixture-secret"},
	} {
		if got, err := resolveServerSelection(input); err == nil || got != "" || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("unsafe choice accepted or reflected: %q %v", got, err)
		}
	}
}
