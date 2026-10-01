package cmd

import (
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodesession"
)

func TestCanKeepFabricEnrollmentWithoutMesh(t *testing.T) {
	tests := []struct {
		name   string
		choice nexus.NetworkChoice
		creds  persistedFabricCredentials
		auth   string
		nexus  string
		want   bool
	}{
		{name: "self-host device API credentials saved", choice: nexus.NetChoiceDevice, creds: fabricCredentialsAPI, auth: "https://aceteam.internal", nexus: "https://nexus.aceteam.internal", want: true},
		{name: "self-host device Redis credentials saved", choice: nexus.NetChoiceDevice, creds: fabricCredentialsRedis, auth: "https://aceteam.internal", nexus: "https://nexus.aceteam.internal", want: true},
		{name: "device credential write failed", choice: nexus.NetChoiceDevice, creds: fabricCredentialsNone, auth: "https://aceteam.internal", nexus: "https://nexus.aceteam.internal", want: false},
		{name: "authkey has no independent fabric enrollment", choice: nexus.NetChoiceAuthkey, creds: fabricCredentialsNone, auth: "https://aceteam.internal", nexus: "https://nexus.aceteam.internal", want: false},
		{name: "authkey cannot inherit API state", choice: nexus.NetChoiceAuthkey, creds: fabricCredentialsAPI, auth: "https://aceteam.internal", nexus: "https://nexus.aceteam.internal", want: false},
		{name: "verified choice does not enroll", choice: nexus.NetChoiceVerified, creds: fabricCredentialsAPI, auth: "https://aceteam.internal", nexus: "https://nexus.aceteam.internal", want: false},
		{name: "managed pair still requires mesh", choice: nexus.NetChoiceDevice, creds: fabricCredentialsAPI, auth: "https://aceteam.ai", nexus: "https://nexus.aceteam.ai", want: false},
		{name: "managed auth with custom Nexus still requires mesh", choice: nexus.NetChoiceDevice, creds: fabricCredentialsAPI, auth: "https://api.aceteam.ai", nexus: "https://nexus.internal", want: false},
		{name: "custom auth with managed Nexus still requires mesh", choice: nexus.NetChoiceDevice, creds: fabricCredentialsAPI, auth: "https://auth.internal", nexus: "https://nexus.aceteam.ai", want: false},
		{name: "invalid endpoint rejected", choice: nexus.NetChoiceDevice, creds: fabricCredentialsAPI, auth: "://bad", nexus: "https://nexus.internal", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canKeepFabricEnrollmentWithoutMesh(tt.choice, tt.creds, tt.auth, tt.nexus); got != tt.want {
				t.Fatalf("canKeepFabricEnrollmentWithoutMesh(%q, %d, %q, %q) = %v, want %v", tt.choice, tt.creds, tt.auth, tt.nexus, got, tt.want)
			}
		})
	}
}

func TestLoginNewDeviceFlag(t *testing.T) {
	flag := loginCmd.Flags().Lookup("new-device")
	if flag == nil {
		t.Fatal("--new-device flag not found")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--new-device default = %q, want false", flag.DefValue)
	}
	if flag.Usage == "" {
		t.Fatal("--new-device flag should have a usage description")
	}

	old := loginNewDevice
	t.Cleanup(func() {
		loginNewDevice = old
		_ = flag.Value.Set("false")
	})
	if err := flag.Value.Set("true"); err != nil {
		t.Fatalf("set --new-device: %v", err)
	}
	if !loginNewDevice {
		t.Fatal("--new-device did not bind the login device-auth force-new option")
	}
}

func TestPersistedFabricCredentialsTransportName(t *testing.T) {
	if got := fabricCredentialsAPI.transportName(); got != "the fabric API" {
		t.Fatalf("API transport name = %q", got)
	}
	if got := fabricCredentialsRedis.transportName(); got != "direct Redis" {
		t.Fatalf("Redis transport name = %q", got)
	}
}

func TestPersistFabricEnrollmentWithoutMesh(t *testing.T) {
	var calls []string
	cfg, err := persistFabricEnrollmentWithoutMesh(
		"https://auth.internal",
		"https://nexus.internal",
		"/node",
		func(authURL, controlURL string) error {
			calls = append(calls, "trust:"+authURL+":"+controlURL)
			return nil
		},
		func(controlURL string) error {
			calls = append(calls, "nexus:"+controlURL)
			return nil
		},
		func(dir string, hasDeviceCredentials bool) (nodesession.Config, error) {
			if !hasDeviceCredentials {
				t.Fatal("meshless device enrollment persisted a non-worker session")
			}
			calls = append(calls, "session:"+dir)
			return nodesession.Config{Mode: nodesession.Worker}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != nodesession.Worker {
		t.Fatalf("session mode = %q, want worker", cfg.Mode)
	}
	want := []string{
		"trust:https://auth.internal:https://nexus.internal",
		"nexus:https://nexus.internal",
		"session:/node",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
}

func TestPersistFabricEnrollmentWithoutMeshFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		trustErr   error
		nexusErr   error
		sessionErr error
		wantCalls  int
	}{
		{name: "private CA", trustErr: errors.New("trust failed"), wantCalls: 1},
		{name: "Nexus URL", nexusErr: errors.New("nexus failed"), wantCalls: 2},
		{name: "session mode", sessionErr: errors.New("session failed"), wantCalls: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			_, err := persistFabricEnrollmentWithoutMesh(
				"https://auth.internal",
				"https://nexus.internal",
				"/node",
				func(string, string) error { calls++; return tt.trustErr },
				func(string) error { calls++; return tt.nexusErr },
				func(string, bool) (nodesession.Config, error) {
					calls++
					return nodesession.Config{}, tt.sessionErr
				},
			)
			if err == nil {
				t.Fatal("persistence failure reported a successful meshless enrollment")
			}
			if calls != tt.wantCalls {
				t.Fatalf("persistence calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func TestPersistFabricEnrollmentWithoutMeshPreservesPresenceIntent(t *testing.T) {
	cfg, err := persistFabricEnrollmentWithoutMesh(
		"https://auth.internal",
		"https://nexus.internal",
		"/node",
		func(string, string) error { return nil },
		func(string) error { return nil },
		func(string, bool) (nodesession.Config, error) {
			return nodesession.Config{Mode: nodesession.Presence}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != nodesession.Presence {
		t.Fatalf("session mode = %q, want preserved presence", cfg.Mode)
	}
}
