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
		want   bool
	}{
		{name: "device API credentials saved", choice: nexus.NetChoiceDevice, creds: fabricCredentialsAPI, want: true},
		{name: "device Redis credentials saved", choice: nexus.NetChoiceDevice, creds: fabricCredentialsRedis, want: true},
		{name: "device credential write failed", choice: nexus.NetChoiceDevice, creds: fabricCredentialsNone, want: false},
		{name: "authkey has no independent fabric enrollment", choice: nexus.NetChoiceAuthkey, creds: fabricCredentialsNone, want: false},
		{name: "authkey cannot inherit API state", choice: nexus.NetChoiceAuthkey, creds: fabricCredentialsAPI, want: false},
		{name: "verified choice does not enroll", choice: nexus.NetChoiceVerified, creds: fabricCredentialsAPI, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canKeepFabricEnrollmentWithoutMesh(tt.choice, tt.creds); got != tt.want {
				t.Fatalf("canKeepFabricEnrollmentWithoutMesh(%q, %d) = %v, want %v", tt.choice, tt.creds, got, tt.want)
			}
		})
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
	err := persistFabricEnrollmentWithoutMesh(
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
			return nodesession.Config{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
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
			err := persistFabricEnrollmentWithoutMesh(
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
