package meshtransfer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
)

// BootstrapPeerCert establishes the trust anchor for later cache-transfer
// requests to a peer. It does two things, strictly in this order:
//
//  1. Verifies the peer's mesh identity FIRST (Verify), so an unverified,
//     wrong-device, or different-owner peer never gets its certificate captured
//     or trusted. If verification fails, no dial to the peer is attempted at all.
//  2. Opens a TLS connection to that peer's EXISTING status endpoint over the
//     mesh-only dial guard and captures the leaf certificate it presents at the
//     handshake.
//
// The captured leaf is trusted on first use precisely BECAUSE the mesh identity
// was independently verified (cryptographic tailnet membership) — not because any
// CA vouches for it (the gateway serves a self-signed certificate). The returned
// VerifiedPeer + leaf are what VerifiedPeer.Client pins for every subsequent
// request, so a later connection presenting a different certificate is rejected.
//
// statusPort is the peer's status/gateway TLS port (the "existing status
// endpoint"). dial is the mesh dialer (production: network.Dial).
//
// want.StableID is REQUIRED here (unlike Verify, where it is optional): pinning a
// certificate is the trust-commitment point, so the caller must name the exact
// device it intends to pin to. A caller learns the device id from a prior
// Verify(meshIP, Expected{}) discovery call. Requiring it makes "IP reuse fails
// closed" unconditional at bootstrap — the peer now at meshIP must be the intended
// device, not merely whatever same-owner peer holds that IP. want.OwnerID stays
// optional (Verify still requires a non-empty owner id regardless).
//
// ctx SHOULD carry a deadline: the TLS handshake blocks until one elapses.
func BootstrapPeerCert(ctx context.Context, resolver PeerResolver, dial Dialer, meshIP string, statusPort int, want Expected) (*VerifiedPeer, *x509.Certificate, error) {
	if dial == nil {
		return nil, nil, errors.New("meshtransfer: nil dialer")
	}
	if statusPort <= 0 || statusPort > 65535 {
		return nil, nil, fmt.Errorf("meshtransfer: invalid status port %d", statusPort)
	}
	if want.StableID == "" {
		return nil, nil, errors.New("meshtransfer: BootstrapPeerCert requires want.StableID (the device to pin to)")
	}

	// 1. Verify identity BEFORE any dial/TLS to the peer. On failure we return
	//    without ever touching the network — the certificate of an unverified
	//    peer is never captured.
	peer, err := Verify(ctx, resolver, meshIP, want)
	if err != nil {
		return nil, nil, err
	}

	// 2. Dial the status endpoint over the mesh-only guard and capture the leaf.
	guarded := meshOnlyDial(peer.MeshIP, dial)
	addr := net.JoinHostPort(peer.MeshIP.String(), strconv.Itoa(statusPort))
	raw, err := guarded(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("meshtransfer: dial status endpoint %s: %w", addr, err)
	}

	// Identity is already verified, so we accept whatever leaf the verified peer
	// presents (trust-on-first-use gated by verified mesh identity). We still pin
	// the mesh ServerName and require at least one certificate.
	var captured *x509.Certificate
	tlsConn := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // trust anchor is the verified mesh identity, not a CA; the leaf is captured here to be pinned by Client
		ServerName:         peer.MeshIP.String(),
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("meshtransfer: peer presented no certificate")
			}
			captured = cs.PeerCertificates[0]
			return nil
		},
	})
	defer tlsConn.Close()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("meshtransfer: tls handshake with status endpoint %s: %w", addr, err)
	}
	if captured == nil {
		return nil, nil, fmt.Errorf("meshtransfer: no certificate captured from status endpoint %s", addr)
	}
	return peer, captured, nil
}
