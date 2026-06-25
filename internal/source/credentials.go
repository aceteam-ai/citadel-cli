package source

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CredentialKind discriminates the auth mechanism a Credential carries.
type CredentialKind int

const (
	// CredNone means no credential — a PUBLIC source cloned anonymously.
	CredNone CredentialKind = iota
	// CredHTTPSToken is an HTTPS access token (deploy token / PAT) cloned over
	// https as `https://x-access-token:<token>@host/...`.
	CredHTTPSToken
	// CredSSHKey is a path to a private SSH key used via GIT_SSH_COMMAND.
	CredSSHKey
)

// String renders a CredentialKind for diagnostics. It never reveals a secret.
func (k CredentialKind) String() string {
	switch k {
	case CredNone:
		return "none"
	case CredHTTPSToken:
		return "https-token"
	case CredSSHKey:
		return "ssh-key"
	default:
		return "unknown"
	}
}

// Credential is a git credential the node clones a private source with.
//
// SECURITY: the secret value (token) is held in an UNEXPORTED field and is never
// rendered by String() / Redacted() / %v / %+v / %#v. Retrieve it explicitly via
// Secret() only at the point of use (building an authenticated clone URL). The
// SSH key path is NOT itself a secret (the key material on disk is), so the path
// is shown; the key file is never read into this struct.
type Credential struct {
	// Kind is the auth mechanism.
	Kind CredentialKind
	// Host is the host this credential authenticates to (e.g. "github.com").
	Host string
	// Username is the HTTPS basic-auth username. Defaults to "x-access-token"
	// (GitHub's convention for token-as-password) when empty. Not a secret.
	Username string
	// KeyPath is the SSH private-key path for CredSSHKey. Not a secret itself.
	KeyPath string

	// token is the HTTPS access token. UNEXPORTED so struct stringification
	// (%v/%+v/%#v) cannot leak it; access only via Secret().
	token string
}

// NoCredential is the zero credential: a public source cloned anonymously.
var NoCredential = Credential{Kind: CredNone}

// IsNone reports whether the credential carries no auth (public source).
func (c Credential) IsNone() bool { return c.Kind == CredNone }

// Secret returns the raw token for an HTTPS-token credential. This is the ONLY
// accessor for the secret; callers must use the value transiently (e.g. to build
// an authenticated clone URL at apply time) and never log or persist it.
func (c Credential) Secret() string { return c.token }

// Redacted returns a log-safe one-line summary that never includes the token.
func (c Credential) Redacted() string {
	switch c.Kind {
	case CredNone:
		return "Credential{none}"
	case CredHTTPSToken:
		user := c.Username
		if user == "" {
			user = defaultHTTPSUser
		}
		return fmt.Sprintf("Credential{https-token host=%s user=%s token=REDACTED}", c.Host, user)
	case CredSSHKey:
		return fmt.Sprintf("Credential{ssh-key host=%s key=%s}", c.Host, c.KeyPath)
	default:
		return "Credential{unknown}"
	}
}

// String implements fmt.Stringer with the redacted form so the token never
// leaks through %v / %s formatting.
func (c Credential) String() string { return c.Redacted() }

// GoString implements fmt.GoStringer so even %#v (which would otherwise dump the
// unexported field via reflection) renders the redacted form.
func (c Credential) GoString() string { return c.Redacted() }

const defaultHTTPSUser = "x-access-token"

// CredentialProvider supplies the git credential a node should clone a given
// source with, keyed by the resolved Descriptor (its Host / CloneURL). A public
// source yields NoCredential; an unknown/unscoped private source also yields
// NoCredential (the clone then fails with catalog's credential-hint error,
// which is the correct, non-leaking behavior).
//
// Implementations MUST NOT log secrets and MUST NOT place secrets in returned
// errors.
type CredentialProvider interface {
	// CredentialFor returns the credential to clone d with, or NoCredential when
	// the source is public or no scoped credential is configured for its host.
	CredentialFor(d Descriptor) (Credential, error)
}

// NoCredentials is the default provider: it supplies no auth, so only PUBLIC
// sources can be cloned. It is the safe baseline for a vanilla node.
type NoCredentials struct{}

// CredentialFor implements CredentialProvider. It always returns NoCredential.
func (NoCredentials) CredentialFor(Descriptor) (Credential, error) {
	return NoCredential, nil
}

// fileCredential is the on-disk JSON shape of one host-scoped credential. The
// token is read transiently and copied into Credential.token; it is never logged.
type fileCredential struct {
	// Host is the host this credential authenticates to (matched against the
	// Descriptor's Host). Optional; defaults to the filename's host slug.
	Host string `json:"host"`
	// Type is "https-token" or "ssh-key".
	Type string `json:"type"`
	// Username is the HTTPS basic-auth username (optional).
	Username string `json:"username,omitempty"`
	// Token is the HTTPS access token (for type=https-token). A SECRET.
	Token string `json:"token,omitempty"`
	// KeyPath is the SSH private-key path (for type=ssh-key).
	KeyPath string `json:"key_path,omitempty"`
}

// FileProvider is a local, file-backed CredentialProvider. It reads scoped git
// credentials from a secure node directory (default ~/citadel-node/credentials/),
// one JSON file per host. Files MUST be 0600; the mode is enforced on READ and a
// too-permissive file is REJECTED before its contents are read, so a rejection
// error can never contain the secret.
//
// This is the node's secure local secret store. The control-plane-PROVISIONED
// path (the plane mints a scoped deploy token / SSH key per node and rotates it)
// is DEFERRED — see ControlPlaneProvider below and aceteam-ai/aceteam#4273.
type FileProvider struct {
	// Dir is the credentials directory. When empty, DefaultCredentialsDir() is
	// used. Injectable so tests can point it at a temp dir.
	Dir string
}

// DefaultCredentialsDir returns the default node credentials directory,
// ~/citadel-node/credentials/. Kept separate from the catalog/config dir so the
// node's git secrets have a single, locked-down home.
func DefaultCredentialsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Fall back to a relative path rather than a guessable absolute one; the
		// caller will fail to read and fall back to NoCredential.
		return filepath.Join("citadel-node", "credentials")
	}
	return filepath.Join(home, "citadel-node", "credentials")
}

func (p FileProvider) dir() string {
	if p.Dir != "" {
		return p.Dir
	}
	return DefaultCredentialsDir()
}

// CredentialFor implements CredentialProvider. It looks up a credential file
// named "<host>.json" (host slugified to a filesystem-safe name) in the
// provider's directory. A missing file or a catalog/public source yields
// NoCredential (not an error): the source is treated as public.
func (p FileProvider) CredentialFor(d Descriptor) (Credential, error) {
	host := d.Host
	if host == "" {
		// Catalog name or unknown host: nothing to authenticate.
		return NoCredential, nil
	}

	path := filepath.Join(p.dir(), hostFileName(host))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No scoped credential for this host: treat as public.
			return NoCredential, nil
		}
		return NoCredential, fmt.Errorf("stat credential for host %q: %w", host, err)
	}

	// Enforce 0600 BEFORE reading, so a rejection can never echo the secret.
	// (Permission bits are not meaningful on Windows; skip the check there.)
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return NoCredential, fmt.Errorf(
				"credential file for host %q has insecure permissions %#o (must be 0600); refusing to read",
				host, perm)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return NoCredential, fmt.Errorf("read credential for host %q: %w", host, err)
	}

	cred, err := parseFileCredential(host, data)
	if err != nil {
		// parseFileCredential is written to never include the token in its error.
		return NoCredential, err
	}
	return cred, nil
}

// hostFileName maps a host to its credential filename, slugifying characters
// that are unsafe in a path segment so a malicious host string cannot escape the
// credentials directory.
func hostFileName(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String() + ".json"
}

// ControlPlaneProvider is the DEFERRED control-plane-provisioned credential
// path: the plane mints a scoped deploy token / SSH key per node (the #342
// private-repo caveat) and rotates it, and this provider fetches/refreshes it
// over the node's authenticated device identity.
//
// It is intentionally a stub — implementing it requires control-plane endpoints
// that DO NOT EXIST YET. See aceteam-ai/aceteam#4273.
//
// TODO(aceteam#4273): implement live provisioning + rotation against the control
// plane, authenticated by node device identity; persist the scoped credential to
// the FileProvider directory (0600) so reconcile can clone private sources.
type ControlPlaneProvider struct{}

// CredentialFor implements CredentialProvider. DEFERRED: it returns an explicit
// not-implemented error referencing aceteam#4273 rather than silently returning
// NoCredential, so a misconfiguration is visible instead of masquerading as a
// public source.
func (ControlPlaneProvider) CredentialFor(Descriptor) (Credential, error) {
	return NoCredential, fmt.Errorf(
		"control-plane-provisioned credentials are not implemented yet (deferred to aceteam-ai/aceteam#4273)")
}
