package meshtransfer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
)

// errRedirectRefused is returned by the client's CheckRedirect. A redirect is a
// covert way to leave the pinned peer, so the transport never follows one.
var errRedirectRefused = errors.New("meshtransfer: redirects are refused")

// meshOnlyDial wraps a mesh Dialer so it refuses any address that is not the
// verified peer's mesh IP. This is what makes the transport dial ONLY the
// verified peer: a redirect target, a public DNS name, an arbitrary URL, a
// non-mesh IP, or any other mesh peer is refused before a single byte leaves the
// process. It never resolves a name (resolving would defeat the IP pin), so a
// DNS-based redirect cannot reach the underlying dialer at all.
func meshOnlyDial(peer netip.Addr, dial Dialer) Dialer {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("meshtransfer: refusing malformed dial address %q", addr)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			// A hostname (public or otherwise) is never dialed: identity is pinned
			// to an IP, and resolving a name would defeat the pin.
			return nil, fmt.Errorf("meshtransfer: refusing non-IP host %q", host)
		}
		ip = ip.Unmap()
		if !isMeshIP(ip) {
			return nil, fmt.Errorf("meshtransfer: refusing non-mesh address %s", ip)
		}
		if ip != peer {
			return nil, fmt.Errorf("meshtransfer: refusing address %s (pinned to verified peer %s)", ip, peer)
		}
		return dial(ctx, network, net.JoinHostPort(ip.String(), port))
	}
}

// pinnedTLSConfig builds a tls.Config that trusts ONLY the exact leaf captured
// over the authenticated mesh connection (BootstrapPeerCert). The trust anchor is
// the verified mesh identity plus this pinned leaf, NOT a CA — the gateway serves
// a self-signed certificate — so default chain verification is disabled and
// replaced by a byte-exact leaf comparison. Any other/rotated/unverified
// certificate is rejected.
func pinnedTLSConfig(peer netip.Addr, leaf *x509.Certificate) *tls.Config {
	return &tls.Config{
		// InsecureSkipVerify disables the DEFAULT (CA-chain + hostname)
		// verification only; VerifyConnection below is the real, stricter check:
		// the presented leaf must be byte-identical to the pinned one.
		InsecureSkipVerify: true, //nolint:gosec // pinned-leaf check in VerifyConnection is stricter than CA verification
		ServerName:         peer.String(),
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("meshtransfer: peer presented no certificate")
			}
			if !bytes.Equal(cs.PeerCertificates[0].Raw, leaf.Raw) {
				return errors.New("meshtransfer: peer certificate does not match the bootstrapped certificate")
			}
			return nil
		},
	}
}

// Client returns an *http.Client that can only ever reach the verified peer:
//
//   - it dials exclusively through the injected mesh dialer to the peer's pinned
//     mesh IP (meshOnlyDial), refusing any other address, hostname, or non-mesh IP;
//   - it uses NO proxy (a self-constructed http.Transport has a nil Proxy, unlike
//     http.DefaultTransport, so HTTP(S)_PROXY is never consulted);
//   - it pins the peer's bootstrapped TLS leaf (pinnedTLSConfig), rejecting any
//     other or unverified certificate;
//   - it never follows a redirect.
//
// Request URLs handed to the returned client MUST use the peer's mesh IP literal
// as the host (e.g. https://100.64.0.9:8080/...). A MagicDNS or any other
// hostname is refused by the dial guard — the transport never resolves a name,
// which is the no-public-DNS property working as intended.
//
// leaf is the certificate returned by BootstrapPeerCert for this same peer. A nil
// dialer or leaf is an error (fail closed rather than dial insecurely).
func (p *VerifiedPeer) Client(dial Dialer, leaf *x509.Certificate) (*http.Client, error) {
	if dial == nil {
		return nil, errors.New("meshtransfer: nil dialer")
	}
	if leaf == nil {
		return nil, errors.New("meshtransfer: nil pinned certificate")
	}
	guarded := meshOnlyDial(p.MeshIP, dial)
	tr := &http.Transport{
		Proxy:             nil, // never use a proxy — stay on the pinned mesh peer
		DialContext:       func(ctx context.Context, network, addr string) (net.Conn, error) { return guarded(ctx, network, addr) },
		TLSClientConfig:   pinnedTLSConfig(p.MeshIP, leaf),
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectRefused
		},
	}, nil
}
