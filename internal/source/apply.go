package source

import (
	"context"
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// AuthenticatedClone is the result of applying a credential to a source: the
// concrete clone URL plus any extra environment the git process needs.
//
// SECURITY: for an HTTPS-token credential the CloneURL embeds the token
// (`https://x-access-token:<token>@host/...`) and is therefore ITSELF A SECRET.
// It must be used transiently to launch git and NEVER logged, persisted to
// .git/config, reported to the control plane, or placed in an error. Use
// Redacted() for any diagnostic output.
type AuthenticatedClone struct {
	// CloneURL is the URL git should clone from. For a token credential this
	// contains the secret; for ssh/none it is the plain URL.
	CloneURL string
	// Env is extra environment for the git process (e.g. GIT_SSH_COMMAND for an
	// ssh-key credential). Never contains the token.
	Env []string
	// kind records how auth was applied, for Redacted().
	kind CredentialKind
	// plainURL is the non-secret URL, for Redacted().
	plainURL string
}

// Redacted returns a log-safe description that never contains the token.
func (a AuthenticatedClone) Redacted() string {
	return fmt.Sprintf("AuthenticatedClone{auth=%s url=%s env=%d}", a.kind, a.plainURL, len(a.Env))
}

// String implements fmt.Stringer with the redacted form.
func (a AuthenticatedClone) String() string { return a.Redacted() }

// GoString implements fmt.GoStringer so %#v cannot dump the token-bearing URL.
func (a AuthenticatedClone) GoString() string { return a.Redacted() }

// ApplyCredential is the PURE, testable heart of credential application: given a
// resolved Descriptor and the Credential selected for it, it produces the
// concrete clone URL + git environment, without performing any I/O or clone.
//
//   - CredNone  -> plain CloneURL, no env (public clone).
//   - CredHTTPSToken over an https:// URL -> token injected into the URL
//     authority as `https://<user>:<token>@host/...`.
//   - CredSSHKey -> plain URL + GIT_SSH_COMMAND pinning the key (and disabling
//     agent/known-host prompts that would hang an unattended node clone).
func ApplyCredential(d Descriptor, cred Credential) (AuthenticatedClone, error) {
	if d.Kind == KindCatalog {
		return AuthenticatedClone{}, fmt.Errorf("apply credential: %q is a catalog name and is not git-cloned", d.Raw)
	}
	if d.CloneURL == "" {
		return AuthenticatedClone{}, fmt.Errorf("apply credential: source %q has no clone URL", d.Raw)
	}

	switch cred.Kind {
	case CredNone:
		return AuthenticatedClone{CloneURL: d.CloneURL, kind: CredNone, plainURL: d.CloneURL}, nil

	case CredHTTPSToken:
		authURL, err := injectHTTPSToken(d.CloneURL, cred.Username, cred.Secret())
		if err != nil {
			return AuthenticatedClone{}, err
		}
		return AuthenticatedClone{CloneURL: authURL, kind: CredHTTPSToken, plainURL: d.CloneURL}, nil

	case CredSSHKey:
		// IdentitiesOnly forces git to use only the provided key; BatchMode +
		// StrictHostKeyChecking=accept-new keep an unattended node clone from
		// blocking on an interactive prompt.
		sshCmd := fmt.Sprintf(
			"ssh -i %s -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new",
			cred.KeyPath)
		return AuthenticatedClone{
			CloneURL: d.CloneURL,
			Env:      []string{"GIT_SSH_COMMAND=" + sshCmd},
			kind:     CredSSHKey,
			plainURL: d.CloneURL,
		}, nil

	default:
		return AuthenticatedClone{}, fmt.Errorf("apply credential: unknown credential kind %d", cred.Kind)
	}
}

// injectHTTPSToken rewrites an https:// (or http://) URL to carry basic-auth
// credentials in its authority: scheme://user:token@host/path. It refuses any
// non-http(s) scheme (a token must not be injected into an ssh/git URL) and any
// URL that already contains userinfo.
func injectHTTPSToken(cloneURL, user, token string) (string, error) {
	var scheme string
	switch {
	case strings.HasPrefix(cloneURL, "https://"):
		scheme = "https://"
	case strings.HasPrefix(cloneURL, "http://"):
		scheme = "http://"
	default:
		return "", fmt.Errorf("cannot inject an HTTPS token into a non-http(s) source URL")
	}
	rest := strings.TrimPrefix(cloneURL, scheme)
	if strings.Contains(rest[:hostEnd(rest)], "@") {
		return "", fmt.Errorf("source URL already carries userinfo; refusing to inject a token")
	}
	if user == "" {
		user = defaultHTTPSUser
	}
	// NOTE: the token is not URL-encoded. GitHub deploy tokens / PATs (the
	// x-access-token convention) are URL-safe (alphanumeric + '_'), so this is
	// correct for the supported case. A non-GitHub token containing reserved
	// authority characters ('@', '/', ':') would need percent-encoding; that is
	// out of scope until a non-GitHub private host is actually supported.
	return scheme + user + ":" + token + "@" + rest, nil
}

// hostEnd returns the index in rest (the part after the scheme) at which the
// authority ends (first '/' '?' or '#'), or len(rest) if none.
func hostEnd(rest string) int {
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		return i
	}
	return len(rest)
}

// Cloner performs the actual clone+manifest-load for a resolved source. It is
// the SEAM that keeps the real git clone injectable: the live adapter wires it
// to catalog.ResolveSource (CatalogCloner below) while tests substitute a fake
// so NO real clone runs against a private repo in CI.
type Cloner interface {
	// Clone fetches the module for the given catalog source — already
	// credential-applied via the provided AuthenticatedClone — and returns the
	// loaded module. Implementations MUST treat auth.CloneURL as a secret.
	Clone(ctx context.Context, src catalog.Source, auth AuthenticatedClone) (*catalog.ResolvedModule, error)
}

// CatalogCloner is the LIVE Cloner: it delegates to catalog.ResolveSource, which
// owns the real clone/update + manifest load. It is a thin adapter so the
// existing #342 install path stays the single place a clone actually happens.
//
// NOTE: catalog.ResolveSource currently derives auth from the ambient git
// environment (GITHUB_TOKEN / ssh / credential helper — see catalog.cloneError).
// Threading an explicit per-source AuthenticatedClone (the rewritten URL +
// GIT_SSH_COMMAND) through ResolveSource is the next increment; until then this
// adapter relies on that ambient path and the AuthenticatedClone documents the
// intended wiring. This is why the SCOPE defers LIVE private clones to a follow-
// up — no real private clone is exercised here or in tests.
type CatalogCloner struct{}

// Clone implements Cloner by delegating to catalog.ResolveSource.
func (CatalogCloner) Clone(_ context.Context, src catalog.Source, _ AuthenticatedClone) (*catalog.ResolvedModule, error) {
	return catalog.ResolveSource(src)
}

// Fetch is the end-to-end node-side resolution helper, wiring resolution +
// credential selection + application + the (injected) clone seam together. It is
// what the live reconcile ModuleOps adapter calls; in tests a fake Cloner makes
// it exercisable WITHOUT a real clone.
//
// It never logs the credential or the authenticated URL.
func Fetch(ctx context.Context, rawSource string, provider CredentialProvider, cloner Cloner) (*catalog.ResolvedModule, error) {
	d, err := Resolve(rawSource)
	if err != nil {
		return nil, err
	}
	if d.IsCatalog() {
		// Catalog names are installed via the existing catalog path, not cloned
		// here; the live adapter handles that branch.
		return nil, fmt.Errorf("source %q is a catalog name; install via the catalog path", rawSource)
	}

	cred, err := provider.CredentialFor(d)
	if err != nil {
		return nil, err
	}
	auth, err := ApplyCredential(d, cred)
	if err != nil {
		return nil, err
	}
	return cloner.Clone(ctx, d.CatalogSource(), auth)
}
