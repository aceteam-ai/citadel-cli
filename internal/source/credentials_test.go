package source

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeToken is an OBVIOUS fake — never a real secret in a fixture.
const fakeToken = "tok_FAKE_do_not_use_1234567890"

func mustResolve(t *testing.T, raw string) Descriptor {
	t.Helper()
	d, err := Resolve(raw)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", raw, err)
	}
	return d
}

// writeCred writes a credential file with the given perms and returns its dir.
func writeCred(t *testing.T, host, content string, perm os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, hostFileName(host))
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write cred: %v", err)
	}
	// os.WriteFile is subject to umask; force the exact mode.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod cred: %v", err)
	}
	return dir
}

func TestNoCredentials_AlwaysNone(t *testing.T) {
	p := NoCredentials{}
	for _, raw := range []string{"embedding", "acme/widgets@v1", "https://git.example.com/a/b.git"} {
		cred, err := p.CredentialFor(mustResolve(t, raw))
		if err != nil {
			t.Fatalf("CredentialFor(%q): %v", raw, err)
		}
		if !cred.IsNone() {
			t.Errorf("CredentialFor(%q) = %v, want none", raw, cred.Kind)
		}
	}
}

func TestFileProvider_Selection(t *testing.T) {
	httpsCred := `{"type":"https-token","token":"` + fakeToken + `"}`
	sshCred := `{"type":"ssh-key","key_path":"/home/citadel/.ssh/deploy_key"}`

	tests := []struct {
		name     string
		host     string // host the cred file is written for ("" = no file)
		content  string
		query    string // source string to resolve+query
		wantKind CredentialKind
	}{
		{
			name:     "public catalog source -> none",
			query:    "embedding",
			wantKind: CredNone,
		},
		{
			name:     "private host with matching https token",
			host:     "git.example.com",
			content:  httpsCred,
			query:    "https://git.example.com/acme/widgets.git",
			wantKind: CredHTTPSToken,
		},
		{
			name:     "private host with matching ssh key",
			host:     "git.example.com",
			content:  sshCred,
			query:    "ssh://git@git.example.com/acme/widgets.git",
			wantKind: CredSSHKey,
		},
		{
			name:     "unknown host (no cred file) -> none",
			host:     "git.example.com",
			content:  httpsCred,
			query:    "https://other.example.org/acme/widgets.git",
			wantKind: CredNone,
		},
		{
			name:     "github owner/repo matches github.com cred",
			host:     "github.com",
			content:  httpsCred,
			query:    "acme/widgets@v1.2.0",
			wantKind: CredHTTPSToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dir string
			if tt.host != "" {
				dir = writeCred(t, tt.host, tt.content, 0o600)
			} else {
				dir = t.TempDir()
			}
			p := FileProvider{Dir: dir}
			cred, err := p.CredentialFor(mustResolve(t, tt.query))
			if err != nil {
				t.Fatalf("CredentialFor(%q): %v", tt.query, err)
			}
			if cred.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", cred.Kind, tt.wantKind)
			}
		})
	}
}

func TestFileProvider_Rejects0600Violation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits not enforced on Windows")
	}
	content := `{"type":"https-token","token":"` + fakeToken + `"}`
	dir := writeCred(t, "git.example.com", content, 0o644) // world-readable: too open

	p := FileProvider{Dir: dir}
	cred, err := p.CredentialFor(mustResolve(t, "https://git.example.com/a/b.git"))
	if err == nil {
		t.Fatal("expected an error for insecure (0644) credential file, got nil")
	}
	if !cred.IsNone() {
		t.Errorf("on rejection cred should be none, got %v", cred.Kind)
	}
	// The rejection error must NOT contain the secret (it is reported before read).
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("rejection error leaked the token: %q", err.Error())
	}
}

func TestFileProvider_MissingFileIsPublic(t *testing.T) {
	p := FileProvider{Dir: t.TempDir()}
	cred, err := p.CredentialFor(mustResolve(t, "https://git.example.com/a/b.git"))
	if err != nil {
		t.Fatalf("CredentialFor: %v", err)
	}
	if !cred.IsNone() {
		t.Errorf("missing cred file should yield none, got %v", cred.Kind)
	}
}

func TestParseFileCredential_Errors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"not json", `not json at all`},
		{"missing type", `{"token":"` + fakeToken + `"}`},
		{"unknown type", `{"type":"oauth","token":"` + fakeToken + `"}`},
		{"empty https token", `{"type":"https-token","token":""}`},
		{"empty ssh key path", `{"type":"ssh-key","key_path":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cred, err := parseFileCredential("github.com", []byte(tt.content))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !cred.IsNone() {
				t.Errorf("error path should return none, got %v", cred.Kind)
			}
			if strings.Contains(err.Error(), fakeToken) {
				t.Fatalf("parse error leaked the token: %q", err.Error())
			}
		})
	}
}

func TestControlPlaneProvider_Deferred(t *testing.T) {
	cred, err := ControlPlaneProvider{}.CredentialFor(mustResolve(t, "https://git.example.com/a/b.git"))
	if err == nil {
		t.Fatal("ControlPlaneProvider should return a not-implemented error")
	}
	if !strings.Contains(err.Error(), "4273") {
		t.Errorf("deferred error should reference aceteam#4273, got %q", err.Error())
	}
	if !cred.IsNone() {
		t.Errorf("deferred cred should be none, got %v", cred.Kind)
	}
}

func TestHostFileName_NoTraversal(t *testing.T) {
	// A malicious host must not escape the credentials directory.
	got := hostFileName("../../etc/passwd")
	if strings.Contains(got, "/") || strings.Contains(got, "\\") {
		t.Fatalf("hostFileName leaked a path separator: %q", got)
	}
}
