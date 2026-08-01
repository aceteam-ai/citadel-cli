// internal/network/backend_test.go
// Exercises NetworkServer against a fake Backend. Before the Backend split
// (issue #643) none of these paths were reachable without a live tsnet server
// and a real coordination server, so they had no unit coverage at all.
package network

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// fakeBackend is a Backend whose every answer is set by the test.
type fakeBackend struct {
	status    *ipnstate.Status
	statusErr error
	ip4, ip6  netip.Addr
	ping      *ipnstate.PingResult
	pingErr   error

	closed      bool
	reauthedKey string
	reauthErr   error
}

func (f *fakeBackend) Up(context.Context) error { return nil }

func (f *fakeBackend) Close() error {
	f.closed = true
	return nil
}

func (f *fakeBackend) Status(context.Context) (*ipnstate.Status, error) {
	return f.status, f.statusErr
}

func (f *fakeBackend) TailscaleIPs() (netip.Addr, netip.Addr) { return f.ip4, f.ip6 }

func (f *fakeBackend) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeBackend) Listen(string, string) (net.Listener, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeBackend) ListenTLS(string, string) (net.Listener, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeBackend) Ping(context.Context, netip.Addr, tailcfg.PingType) (*ipnstate.PingResult, error) {
	return f.ping, f.pingErr
}

func (f *fakeBackend) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeBackend) Reauth(_ context.Context, authKey string) error {
	f.reauthedKey = authKey
	return f.reauthErr
}

// Compile-time proof that both implementations satisfy the contract. If a
// future TUN backend drifts, this fails at build time rather than at runtime
// on a node.
var (
	_ Backend = (*fakeBackend)(nil)
	_ Backend = (*userspaceBackend)(nil)
	_ Backend = (*tunBackend)(nil)
	_ Backend = (*attachedBackend)(nil)
)

// The authkey is the one field a status/reconnect check can never exercise:
// both VerifyOrReconnect and EnsureConnected call Connect(ctx, ""), so a node
// that reconnects from saved state goes green whether or not the key is
// plumbed through. Drop it and only `citadel login`, `citadel init --authkey`
// and ReconnectWithAuthKey break — silently. Pin the construction instead.
func TestUserspaceBackendCarriesConfig(t *testing.T) {
	b := newUserspaceBackend(ServerConfig{
		Hostname:   "node-1",
		ControlURL: "https://nexus.example",
	}, "/tmp/citadel-state", "hskey-abc")

	if got := b.srv.AuthKey; got != "hskey-abc" {
		t.Errorf("AuthKey = %q, want hskey-abc", got)
	}
	if got := b.srv.Dir; got != "/tmp/citadel-state" {
		t.Errorf("Dir = %q, want /tmp/citadel-state", got)
	}
	if got := b.srv.Hostname; got != "node-1" {
		t.Errorf("Hostname = %q, want node-1", got)
	}
	if got := b.srv.ControlURL; got != "https://nexus.example" {
		t.Errorf("ControlURL = %q, want https://nexus.example", got)
	}
	// Citadel nodes are persistent: an ephemeral node is removed from the
	// coordination server on disconnect and comes back with a new identity.
	if b.srv.Ephemeral {
		t.Error("Ephemeral = true, want false")
	}
}

// Interactive login passes no authkey and relies on the device auth flow.
// An empty string must stay empty rather than becoming a sentinel.
func TestUserspaceBackendEmptyAuthKey(t *testing.T) {
	b := newUserspaceBackend(ServerConfig{Hostname: "node-1"}, "/tmp/citadel-state", "")
	if b.srv.AuthKey != "" {
		t.Errorf("AuthKey = %q, want empty", b.srv.AuthKey)
	}
}

func serverWith(b Backend) *NetworkServer {
	return &NetworkServer{backend: b, connected: true, controlURL: "https://nexus.example"}
}

func TestIsConnectedTracksBackendState(t *testing.T) {
	tests := []struct {
		name    string
		backend *fakeBackend
		want    bool
	}{
		{"running", &fakeBackend{status: &ipnstate.Status{BackendState: "Running"}}, true},
		{"needs login", &fakeBackend{status: &ipnstate.Status{BackendState: "NeedsLogin"}}, false},
		{"stopped", &fakeBackend{status: &ipnstate.Status{BackendState: "Stopped"}}, false},
		// A backend that cannot answer is not connected — never assume up.
		{"status error", &fakeBackend{statusErr: errors.New("boom")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serverWith(tt.backend).IsConnected(); got != tt.want {
				t.Errorf("IsConnected() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsConnectedFalseWithoutBackend(t *testing.T) {
	s := &NetworkServer{}
	if s.IsConnected() {
		t.Error("IsConnected() = true with no backend, want false")
	}
}

func TestGetIPv4(t *testing.T) {
	s := serverWith(&fakeBackend{ip4: netip.MustParseAddr("100.64.0.7")})
	got, err := s.GetIPv4()
	if err != nil {
		t.Fatalf("GetIPv4() error = %v", err)
	}
	if got != "100.64.0.7" {
		t.Errorf("GetIPv4() = %q, want 100.64.0.7", got)
	}

	// No netmap yet: an unassigned address is an error, not an empty string a
	// caller might print as if it were real.
	if _, err := serverWith(&fakeBackend{}).GetIPv4(); err == nil {
		t.Error("GetIPv4() with no assigned address: want error, got nil")
	}
}

func TestStatusSummarizesBackend(t *testing.T) {
	s := serverWith(&fakeBackend{
		ip4: netip.MustParseAddr("100.64.0.7"),
		status: &ipnstate.Status{
			BackendState: "Running",
			Self: &ipnstate.PeerStatus{
				ID:      tailcfg.StableNodeID("1234"),
				DNSName: "jmbp16-m1pro-8gluaaom.tailnet.ts.net.",
			},
		},
	})

	got, err := s.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !got.Connected {
		t.Error("Connected = false, want true")
	}
	// The registered name wins over the requested hostname, and only the
	// first DNS label is kept.
	if got.Hostname != "jmbp16-m1pro-8gluaaom" {
		t.Errorf("Hostname = %q, want jmbp16-m1pro-8gluaaom", got.Hostname)
	}
	if got.NodeID != "1234" {
		t.Errorf("NodeID = %q, want 1234", got.NodeID)
	}
	if got.IPv4 != "100.64.0.7" {
		t.Errorf("IPv4 = %q, want 100.64.0.7", got.IPv4)
	}
}

func TestStatusWithoutBackendReportsDisconnected(t *testing.T) {
	got, err := (&NetworkServer{}).Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got.Connected {
		t.Error("Connected = true with no backend, want false")
	}
}

func TestGetPeersFiltersToSameUser(t *testing.T) {
	const selfUser = tailcfg.UserID(42)
	mine := key.NewNode().Public()
	theirs := key.NewNode().Public()

	s := serverWith(&fakeBackend{status: &ipnstate.Status{
		Self: &ipnstate.PeerStatus{UserID: selfUser},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			mine: {
				HostName:     "citadel3090",
				UserID:       selfUser,
				Online:       true,
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.5")},
			},
			theirs: {
				HostName: "someone-elses-node",
				UserID:   tailcfg.UserID(99),
				Online:   true,
			},
		},
	}})

	peers, err := s.GetPeers(context.Background())
	if err != nil {
		t.Fatalf("GetPeers() error = %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1 (other users must be filtered out)", len(peers))
	}
	if peers[0].Hostname != "citadel3090" {
		t.Errorf("peer = %q, want citadel3090", peers[0].Hostname)
	}
	if peers[0].IP != "100.64.0.5" {
		t.Errorf("peer IP = %q, want 100.64.0.5", peers[0].IP)
	}
}

// A self with no UserID (netmap not yet populated) must not silently hide
// every peer — GetPeers falls back to showing all of them.
func TestGetPeersFallsBackWhenSelfUnknown(t *testing.T) {
	s := serverWith(&fakeBackend{status: &ipnstate.Status{
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {HostName: "a", UserID: tailcfg.UserID(1)},
			key.NewNode().Public(): {HostName: "b", UserID: tailcfg.UserID(2)},
		},
	}})

	peers, err := s.GetPeers(context.Background())
	if err != nil {
		t.Fatalf("GetPeers() error = %v", err)
	}
	if len(peers) != 2 {
		t.Errorf("got %d peers, want 2", len(peers))
	}
}

func TestPingPeerClassifiesPath(t *testing.T) {
	tests := []struct {
		name         string
		result       *ipnstate.PingResult
		wantConnType string
		wantRelay    string
	}{
		{
			name:         "derp relayed",
			result:       &ipnstate.PingResult{LatencySeconds: 0.025, DERPRegionCode: "sea"},
			wantConnType: "relay",
			wantRelay:    "sea",
		},
		{
			name:         "direct",
			result:       &ipnstate.PingResult{LatencySeconds: 0.004, Endpoint: "1.2.3.4:41641"},
			wantConnType: "direct",
		},
		{
			// Neither field set: report latency but claim nothing about the
			// path rather than guessing "direct".
			name:         "unknown path",
			result:       &ipnstate.PingResult{LatencySeconds: 0.01},
			wantConnType: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := serverWith(&fakeBackend{ping: tt.result})
			latency, connType, relay, err := s.PingPeer(context.Background(), "100.64.0.9")
			if err != nil {
				t.Fatalf("PingPeer() error = %v", err)
			}
			if want := tt.result.LatencySeconds * 1000; latency != want {
				t.Errorf("latency = %v, want %v", latency, want)
			}
			if connType != tt.wantConnType {
				t.Errorf("connType = %q, want %q", connType, tt.wantConnType)
			}
			if relay != tt.wantRelay {
				t.Errorf("relay = %q, want %q", relay, tt.wantRelay)
			}
		})
	}
}

func TestPingPeerRejectsMalformedAddress(t *testing.T) {
	s := serverWith(&fakeBackend{ping: &ipnstate.PingResult{}})
	if _, _, _, err := s.PingPeer(context.Background(), "not-an-ip"); err == nil {
		t.Error("PingPeer(\"not-an-ip\"): want error, got nil")
	}
}

func TestReauthPassesKeyToBackend(t *testing.T) {
	fake := &fakeBackend{}
	if err := serverWith(fake).Reauth(context.Background(), "hskey-fresh"); err != nil {
		t.Fatalf("Reauth() error = %v", err)
	}
	if fake.reauthedKey != "hskey-fresh" {
		t.Errorf("backend received key %q, want hskey-fresh", fake.reauthedKey)
	}
}

func TestReauthWithoutBackendErrors(t *testing.T) {
	if err := (&NetworkServer{}).Reauth(context.Background(), "hskey-fresh"); err == nil {
		t.Error("Reauth() with no backend: want error, got nil")
	}
}

func TestDisconnectClosesBackend(t *testing.T) {
	fake := &fakeBackend{}
	s := serverWith(fake)
	if err := s.Disconnect(); err != nil {
		t.Fatalf("Disconnect() error = %v", err)
	}
	if !fake.closed {
		t.Error("backend was not closed")
	}
	if s.backend != nil {
		t.Error("backend reference survived Disconnect")
	}
}
