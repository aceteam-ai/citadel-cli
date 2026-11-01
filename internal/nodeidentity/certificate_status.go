package nodeidentity

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"time"
)

// CertificateStatus is local diagnostic evidence, not CA trust or revocation
// verification. Nil presence or match means the observation was unavailable,
// not evidence that an entry is absent or that its key differs.
type CertificateStatus struct {
	LeafPresent       *bool      `json:"leaf_present"`
	SigningKeyPresent *bool      `json:"signing_key_present"`
	SPKIMatchesSigner *bool      `json:"spki_matches_signer,omitempty"`
	State             string     `json:"state"`
	NotAfter          *time.Time `json:"not_after,omitempty"`
}

// InspectCertificate never generates a key. Convergent stores retain their
// existing read-through adoption of a previously registered legacy identity.
// Errors are represented by static states, without key paths or PEM material.
func (s *Store) InspectCertificate(now time.Time) CertificateStatus {
	report := CertificateStatus{State: "missing_leaf"}
	if s == nil {
		report.State = "unavailable"
		return report
	}
	public, keyErr := s.PublicKey()
	report.SigningKeyPresent = identityFilePresence(s.KeyPath())
	if keyErr == nil && public != nil {
		present := true
		report.SigningKeyPresent = &present
	}
	leaf, leafErr := s.LoadLeaf()
	report.LeafPresent = identityFilePresence(s.LeafPath())
	if leafErr != nil {
		if errors.Is(leafErr, os.ErrNotExist) {
			return report
		}
		var pathError *os.PathError
		if errors.As(leafErr, &pathError) {
			report.State = "leaf_unavailable"
		} else {
			report.State = "invalid_leaf"
		}
		return report
	}
	present := true
	report.LeafPresent = &present
	expiry := leaf.NotAfter
	report.NotAfter = &expiry
	if keyErr != nil || public == nil {
		report.State = "signing_key_unavailable"
		return report
	}
	spki, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		report.State = "signing_key_unavailable"
		return report
	}
	matches := bytes.Equal(spki, leaf.RawSubjectPublicKeyInfo)
	report.SPKIMatchesSigner = &matches
	switch {
	case !matches:
		report.State = "spki_mismatch"
	case now.Before(leaf.NotBefore):
		report.State = "not_yet_valid"
	case !now.Before(leaf.NotAfter):
		report.State = "expired_leaf"
	default:
		report.State = "local_match"
	}
	return report
}

func (s CertificateStatus) Summary() string {
	switch s.State {
	case "missing_leaf":
		return "missing certificate"
	case "leaf_unavailable":
		return "certificate cannot be read; SPKI match unknown"
	case "invalid_leaf":
		return "invalid certificate; SPKI match unknown"
	case "signing_key_unavailable":
		return "certificate present; signing key unavailable"
	case "spki_mismatch":
		return "certificate present; SPKI does not match signing key"
	case "expired_leaf":
		return "expired certificate; SPKI matches signing key"
	case "not_yet_valid":
		return "certificate not yet valid; SPKI matches signing key"
	case "local_match":
		return "certificate present; SPKI matches signing key"
	default:
		return "certificate comparison unavailable"
	}
}

func identityFilePresence(path string) *bool {
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil
	}
	present := err == nil
	return &present
}
