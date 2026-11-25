package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/network"
)

func TestMeshPeerVerifier(t *testing.T) {
	ok := &network.PeerIdentity{SameOwner: true, StableID: "n1", OwnerID: "7"}
	cases := []struct {
		name string
		id   *network.PeerIdentity
		err  error
		want bool
	}{
		{"verified same-owner", ok, nil, true},
		{"whois error", nil, errors.New("no"), false},
		{"nil identity", nil, nil, false},
		{"other owner", &network.PeerIdentity{StableID: "n1", OwnerID: "7"}, nil, false},
		{"missing stable id", &network.PeerIdentity{SameOwner: true, OwnerID: "7"}, nil, false},
		{"missing owner id", &network.PeerIdentity{SameOwner: true, StableID: "n1"}, nil, false},
	}
	for _, c := range cases {
		v := meshPeerVerifier(func(context.Context, string) (*network.PeerIdentity, error) { return c.id, c.err })
		if got := v(context.Background(), "100.64.0.9:1"); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestStatusBindLAN(t *testing.T) {
	t.Setenv("CITADEL_STATUS_BIND_LAN", "")
	if statusBindLAN(false) {
		t.Fatal("default must not bind LAN")
	}
	if !statusBindLAN(true) {
		t.Fatal("flag must opt in")
	}
	t.Setenv("CITADEL_STATUS_BIND_LAN", "1")
	if !statusBindLAN(false) {
		t.Fatal("env must opt in")
	}
	t.Setenv("CITADEL_STATUS_BIND_LAN", "0")
	if statusBindLAN(false) {
		t.Fatal("0 must not opt in")
	}
}
