package jobs

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
)

// newAMTPinnedTLSConfig is the offline transport primitive for #13768. The pin
// must come from the authenticated binding, never job payload or first contact.
// There is deliberately no runtime caller until that wire contract is agreed.
// The trust anchor is the exact bound leaf certificate, including self-signed
// firmware certificates, rather than a CA or unauthenticated discovery.
func newAMTPinnedTLSConfig(host string, pin [sha256.Size]byte) (*tls.Config, error) {
	if !amtLANAddress(host) || pin == ([sha256.Size]byte{}) {
		return nil, errors.New("AMT binding certificate pin unavailable")
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         host,
		InsecureSkipVerify: true, //nolint:gosec // exact binding certificate pin is enforced by VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || cs.PeerCertificates[0] == nil || len(cs.PeerCertificates[0].Raw) == 0 {
				return errors.New("AMT TLS peer certificate unavailable")
			}
			presented := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(presented[:], pin[:]) != 1 {
				return errors.New("AMT TLS peer certificate does not match binding")
			}
			return nil
		},
	}, nil
}
