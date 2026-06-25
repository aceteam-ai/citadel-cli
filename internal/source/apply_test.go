package source

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// httpsTokenCred builds an https-token Credential with the obvious fake token.
func httpsTokenCred(host string) Credential {
	return Credential{Kind: CredHTTPSToken, Host: host, Username: defaultHTTPSUser, token: fakeToken}
}

// TestCredential_RedactionAllVerbs proves the token never leaks through any of
// the common fmt verbs — %v, %+v, %#v, %s — nor through String()/GoString().
func TestCredential_RedactionAllVerbs(t *testing.T) {
	cred := httpsTokenCred("github.com")

	checks := map[string]string{
		"%v":          fmt.Sprintf("%v", cred),
		"%+v":         fmt.Sprintf("%+v", cred),
		"%#v":         fmt.Sprintf("%#v", cred),
		"%s":          fmt.Sprintf("%s", cred),
		"String()":    cred.String(),
		"GoString()":  cred.GoString(),
		"Redacted()":  cred.Redacted(),
		"in-slice %v": fmt.Sprintf("%v", []Credential{cred}),
		"pointer %v":  fmt.Sprintf("%v", &cred),
	}
	for verb, out := range checks {
		if strings.Contains(out, fakeToken) {
			t.Errorf("%s leaked the token: %q", verb, out)
		}
		if !strings.Contains(out, "REDACTED") && verb != "in-slice %v" && verb != "pointer %v" {
			// in-slice/pointer formatting may elide the marker; the no-leak
			// assertion above is the load-bearing one.
			t.Errorf("%s did not contain REDACTED marker: %q", verb, out)
		}
	}

	// The secret IS retrievable via the explicit accessor (and only that).
	if cred.Secret() != fakeToken {
		t.Errorf("Secret() = %q, want the token", cred.Secret())
	}
}

func TestApplyCredential(t *testing.T) {
	t.Run("none -> plain url", func(t *testing.T) {
		d := mustResolve(t, "https://git.example.com/a/b.git")
		auth, err := ApplyCredential(d, NoCredential)
		if err != nil {
			t.Fatal(err)
		}
		if auth.CloneURL != d.CloneURL {
			t.Errorf("CloneURL = %q, want plain %q", auth.CloneURL, d.CloneURL)
		}
		if len(auth.Env) != 0 {
			t.Errorf("Env = %v, want empty", auth.Env)
		}
	})

	t.Run("https token injected into authority", func(t *testing.T) {
		d := mustResolve(t, "https://git.example.com/a/b.git")
		auth, err := ApplyCredential(d, httpsTokenCred("git.example.com"))
		if err != nil {
			t.Fatal(err)
		}
		want := "https://x-access-token:" + fakeToken + "@git.example.com/a/b.git"
		if auth.CloneURL != want {
			t.Errorf("CloneURL = %q, want %q", auth.CloneURL, want)
		}
	})

	t.Run("ssh key -> GIT_SSH_COMMAND, plain url", func(t *testing.T) {
		d := mustResolve(t, "ssh://git@git.example.com/a/b.git")
		cred := Credential{Kind: CredSSHKey, Host: "git.example.com", KeyPath: "/keys/deploy"}
		auth, err := ApplyCredential(d, cred)
		if err != nil {
			t.Fatal(err)
		}
		if auth.CloneURL != d.CloneURL {
			t.Errorf("CloneURL = %q, want plain %q", auth.CloneURL, d.CloneURL)
		}
		if len(auth.Env) != 1 || !strings.Contains(auth.Env[0], "GIT_SSH_COMMAND=") ||
			!strings.Contains(auth.Env[0], "/keys/deploy") {
			t.Errorf("Env = %v, want GIT_SSH_COMMAND with the key path", auth.Env)
		}
	})

	t.Run("catalog source is not cloneable", func(t *testing.T) {
		d := mustResolve(t, "embedding")
		if _, err := ApplyCredential(d, NoCredential); err == nil {
			t.Fatal("expected error applying credential to a catalog name")
		}
	})

	t.Run("token refused on non-https url", func(t *testing.T) {
		d := mustResolve(t, "ssh://git@git.example.com/a/b.git")
		if _, err := ApplyCredential(d, httpsTokenCred("git.example.com")); err == nil {
			t.Fatal("expected error injecting an HTTPS token into an ssh url")
		}
	})
}

// TestAuthenticatedClone_Redaction proves the token-bearing clone URL never
// leaks through the AuthenticatedClone's stringification.
func TestAuthenticatedClone_Redaction(t *testing.T) {
	d := mustResolve(t, "https://git.example.com/a/b.git")
	auth, err := ApplyCredential(d, httpsTokenCred("git.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the raw struct field does carry the secret (it must, to clone).
	if !strings.Contains(auth.CloneURL, fakeToken) {
		t.Fatal("test precondition: authenticated URL should carry the token")
	}
	for _, out := range []string{
		fmt.Sprintf("%v", auth),
		fmt.Sprintf("%+v", auth),
		fmt.Sprintf("%#v", auth),
		fmt.Sprintf("%s", auth),
		auth.String(),
		auth.Redacted(),
	} {
		if strings.Contains(out, fakeToken) {
			t.Errorf("AuthenticatedClone stringification leaked the token: %q", out)
		}
	}
}

// fakeCloner records the auth it was handed without performing a real clone.
type fakeCloner struct {
	gotURL string
	gotEnv []string
}

func (f *fakeCloner) Clone(_ context.Context, src catalog.Source, auth AuthenticatedClone) (*catalog.ResolvedModule, error) {
	f.gotURL = auth.CloneURL
	f.gotEnv = auth.Env
	return &catalog.ResolvedModule{CacheDir: src.CloneURL}, nil
}

// TestFetch_WiresThroughSeam proves Fetch resolves + selects a credential +
// applies it + hands the authenticated clone to the injected Cloner — WITHOUT a
// real clone (the fake stands in).
func TestFetch_WiresThroughSeam(t *testing.T) {
	content := `{"type":"https-token","token":"` + fakeToken + `"}`
	dir := writeCred(t, "git.example.com", content, 0o600)
	provider := FileProvider{Dir: dir}
	cloner := &fakeCloner{}

	res, err := Fetch(context.Background(), "https://git.example.com/a/b.git", provider, cloner)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res == nil {
		t.Fatal("Fetch returned nil module")
	}
	want := "https://x-access-token:" + fakeToken + "@git.example.com/a/b.git"
	if cloner.gotURL != want {
		t.Errorf("cloner got URL %q, want authenticated %q", cloner.gotURL, want)
	}
}

func TestFetch_CatalogSourceRejected(t *testing.T) {
	cloner := &fakeCloner{}
	if _, err := Fetch(context.Background(), "embedding", NoCredentials{}, cloner); err == nil {
		t.Fatal("Fetch of a catalog name should return an error (use the catalog path)")
	}
}

func TestFetch_PublicSourceNoCreds(t *testing.T) {
	cloner := &fakeCloner{}
	_, err := Fetch(context.Background(), "https://git.example.com/a/b.git", NoCredentials{}, cloner)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cloner.gotURL != "https://git.example.com/a/b.git" {
		t.Errorf("public clone URL = %q, want plain", cloner.gotURL)
	}
}
