package cmd

import (
	"crypto/ecdsa"
	"fmt"
	"io"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

// Each request retains the exact public key that signed its CSR. A later grant
// or a replacement key on disk cannot supply a different enrollment identity.
type deviceAuthIdentityAttempt struct {
	store  *nodeidentity.Store
	csr    string
	public *ecdsa.PublicKey
}

func prepareDeviceAuthIdentity() *deviceAuthIdentityAttempt {
	attempt := &deviceAuthIdentityAttempt{store: loginIdentityStore()}
	csr, public, err := generateLoginCSR(attempt.store)
	if err != nil {
		Debug("device CSR unavailable (non-fatal, continuing without identity enrollment)")
		return attempt
	}
	attempt.csr, attempt.public = csr, public
	return attempt
}

// Only an accepted token may reach this method. Identity enrollment is additive:
// invalid, partial or unavailable bundles never fail the device authorization.
// No filesystem error or key path is included in its diagnostics.
func (attempt *deviceAuthIdentityAttempt) accept(token *nexus.TokenResponse) string {
	if token == nil {
		return ""
	}
	outcome, err := persistLoginIdentityBundle(attempt.store, attempt.public, token.LeafPem, token.ChainPem, token.NodeUID)
	uid := ""
	if err != nil {
		Debug("device identity bundle rejected (non-fatal)")
	} else {
		uid = outcome.NodeUID
	}
	if uid != "" {
		if saveLoginNodeUID(uid) != nil {
			Debug("device serving identity could not be saved (non-fatal)")
			return ""
		}
	} else if clearLoginNodeUID() != nil {
		Debug("previous device serving identity could not be cleared (non-fatal)")
	}
	return uid
}

func deviceEnrollmentHostname(result *DeviceAuthResult, displayName string) string {
	if result == nil {
		return displayName
	}
	return servingIdentityHostname(result.identityNodeUID, displayName)
}

// The post-provisioning path must finish the current device grant in-process.
// An authkey-only child login would discard its verified UID and link context.
func connectDeviceEnrollmentAsWith(result *DeviceAuthResult, displayName string, connect func(string, string, string) error, link func(*DeviceAuthResult) error, disconnect func() error) error {
	if result == nil {
		return deviceEnrollmentRecoveryError(nexus.NewDeviceLinkFailure(nexus.DeviceLinkLocalPrecondition, 0))
	}
	current := *result
	if result.Token != nil {
		token := *result.Token
		current.Token = &token
	}
	return ccConnectDeviceEnrollmentWith(current.Token, current.DeviceCode, func() error {
		return connect(displayName, deviceEnrollmentHostname(&current, displayName), current.Token.Authkey)
	}, link, disconnect)
}

var deviceAuthorizationWithCSR = runDeviceAuthFlowWithCSR

func readNodeCertificateStatus() nodeidentity.CertificateStatus {
	return loginIdentityStore().InspectCertificate(time.Now())
}

func renderNodeCertificateStatus(w io.Writer, status *nodeidentity.CertificateStatus) {
	if status == nil {
		return
	}
	headerColor.Fprintln(w, "\nSIGNING CERTIFICATE")
	fmt.Fprintln(w, "  "+status.Summary())
	fmt.Fprintln(w, "  Local key comparison only; CA trust and revocation were not checked.")
}
