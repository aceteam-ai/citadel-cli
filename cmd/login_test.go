package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/network"
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

func TestSelectLoginNetworkChoiceNewDeviceBypassesExistingState(t *testing.T) {
	called := false
	choice, key, err := selectLoginNetworkChoice(true, func(string) (nexus.NetworkChoice, string, error) {
		called = true
		return nexus.NetChoiceVerified, "stale", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("--new-device consulted existing network state")
	}
	if choice != nexus.NetChoiceDevice || key != "" {
		t.Fatalf("choice, key = %q, %q; want device auth with no key", choice, key)
	}
}

func TestSelectLoginNetworkChoiceDefaultDelegates(t *testing.T) {
	called := false
	choice, key, err := selectLoginNetworkChoice(false, func(authkey string) (nexus.NetworkChoice, string, error) {
		called = true
		if authkey != "" {
			t.Fatalf("authkey = %q, want empty", authkey)
		}
		return nexus.NetChoiceVerified, "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called || choice != nexus.NetChoiceVerified || key != "" {
		t.Fatalf("called, choice, key = %v, %q, %q", called, choice, key)
	}
}

func TestValidateLoginOptions(t *testing.T) {
	if err := validateLoginOptions("", false, false); err != nil {
		t.Fatalf("default options: %v", err)
	}
	if err := validateLoginOptions("", false, true); err != nil {
		t.Fatalf("new-device only: %v", err)
	}
	if err := validateLoginOptions("key", false, false); err != nil {
		t.Fatalf("authkey only: %v", err)
	}
	if err := validateLoginOptions("", true, false); err != nil {
		t.Fatalf("authkey-stdin only: %v", err)
	}
	if err := validateLoginOptions("key", false, true); err == nil {
		t.Fatal("--authkey with --new-device was accepted")
	}
	if err := validateLoginOptions("key", true, false); err == nil {
		t.Fatal("--authkey with --authkey-stdin was accepted")
	}
	if err := validateLoginOptions("", true, true); err == nil {
		t.Fatal("--authkey-stdin with --new-device was accepted")
	}
}

func TestReadLoginAuthkeyStdin(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		terminal bool
		want     string
		wantErr  bool
	}{
		{name: "one token", input: "tskey-auth-example\n", want: "tskey-auth-example"},
		{name: "empty", input: " \n\t", wantErr: true},
		{name: "embedded whitespace", input: "tskey-auth bad", wantErr: true},
		{name: "non-breaking space", input: "tskey-auth-\u00a0bad", wantErr: true},
		{name: "leading non-breaking space", input: "\u00a0tskey-auth-bad\n", wantErr: true},
		{name: "trailing non-breaking space", input: "tskey-auth-bad\u00a0\n", wantErr: true},
		{name: "C1 control", input: "tskey-auth-\u0085bad", wantErr: true},
		{name: "leading C1 NEL", input: "\u0085tskey-auth-bad\n", wantErr: true},
		{name: "trailing C1 NEL", input: "tskey-auth-bad\u0085\n", wantErr: true},
		{name: "extra newline", input: "tskey-auth-bad\n\n", wantErr: true},
		{name: "bare carriage return", input: "tskey-auth-bad\r", wantErr: true},
		{name: "bidi format", input: "tskey-auth-\u202ebad", wantErr: true},
		{name: "punctuation outside token alphabet", input: "tskey-auth-bad!", wantErr: true},
		{name: "terminal refused", input: "tskey-auth-example", terminal: true, wantErr: true},
		{name: "bounded", input: strings.Repeat("x", maxStdinAuthkeyBytes+1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readLoginAuthkeyStdin(strings.NewReader(tt.input), tt.terminal)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("key = %q, want %q", got, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), tt.input) && tt.input != "" {
				t.Fatalf("error exposed input: %v", err)
			}
		})
	}
}

func TestConnectLoginNetwork(t *testing.T) {
	logoutErr := errors.New("state retirement failed")
	connectErr := errors.New("mesh connect failed")
	wantServer := &network.NetworkServer{}
	tests := []struct {
		name          string
		newDevice     bool
		logoutErr     error
		connectErr    error
		wantCalls     []string
		wantAttempted bool
		wantServer    *network.NetworkServer
		wantErr       error
	}{
		{name: "force-new retirement failure stops before connect", newDevice: true, logoutErr: logoutErr, wantCalls: []string{"logout"}, wantErr: logoutErr},
		{name: "force-new successful retirement connects", newDevice: true, wantCalls: []string{"logout", "connect"}, wantAttempted: true, wantServer: wantServer},
		{name: "legacy logout failure remains best effort", logoutErr: logoutErr, wantCalls: []string{"logout", "connect"}, wantAttempted: true, wantServer: wantServer},
		{name: "connect failure remains an attempted mesh connection", newDevice: true, connectErr: connectErr, wantCalls: []string{"logout", "connect"}, wantAttempted: true, wantErr: connectErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			srv, attempted, err := connectLoginNetwork(
				tt.newDevice,
				func() error {
					calls = append(calls, "logout")
					return tt.logoutErr
				},
				func() (*network.NetworkServer, error) {
					calls = append(calls, "connect")
					if tt.connectErr != nil {
						return nil, tt.connectErr
					}
					return wantServer, nil
				},
			)
			if attempted != tt.wantAttempted {
				t.Fatalf("attempted = %v, want %v", attempted, tt.wantAttempted)
			}
			if srv != tt.wantServer {
				t.Fatalf("server = %p, want %p", srv, tt.wantServer)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if len(calls) != len(tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, tt.wantCalls)
			}
			for i := range tt.wantCalls {
				if calls[i] != tt.wantCalls[i] {
					t.Fatalf("calls = %v, want %v", calls, tt.wantCalls)
				}
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
