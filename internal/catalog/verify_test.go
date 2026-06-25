package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestPublisherVerifyMode(t *testing.T) {
	tests := []struct {
		name string
		pub  VerifiedPublisher
		want cosignVerifyMode
	}{
		{"key wins", VerifiedPublisher{Key: "k", Identity: "i"}, modeKeyful},
		{"keyful", VerifiedPublisher{Key: "/path/cosign.pub"}, modeKeyful},
		{"keyless", VerifiedPublisher{Identity: "me@example.com", Issuer: "https://x"}, modeKeyless},
		{"none", VerifiedPublisher{}, modeNone},
		{"blank key+id", VerifiedPublisher{Key: "  ", Identity: "  "}, modeNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := publisherVerifyMode(tt.pub); got != tt.want {
				t.Errorf("publisherVerifyMode(%+v) = %v, want %v", tt.pub, got, tt.want)
			}
		})
	}
}

func TestBuildCosignArgs(t *testing.T) {
	tests := []struct {
		name    string
		pub     VerifiedPublisher
		ref     string
		want    []string
		wantErr bool
	}{
		{
			name: "keyful",
			pub:  VerifiedPublisher{Key: "/k/cosign.pub"},
			ref:  "ghcr.io/o/r@sha256:abc",
			want: []string{"verify", "--key", "/k/cosign.pub", "--", "ghcr.io/o/r@sha256:abc"},
		},
		{
			name: "keyless",
			pub:  VerifiedPublisher{Identity: "me@example.com", Issuer: "https://accounts.example.com"},
			ref:  "ghcr.io/o/r@sha256:abc",
			want: []string{"verify", "--certificate-identity", "me@example.com",
				"--certificate-oidc-issuer", "https://accounts.example.com", "--", "ghcr.io/o/r@sha256:abc"},
		},
		{
			name:    "keyless missing issuer",
			pub:     VerifiedPublisher{Identity: "me@example.com"},
			ref:     "img",
			wantErr: true,
		},
		{
			name:    "no key no identity",
			pub:     VerifiedPublisher{Pattern: "o/r"},
			ref:     "img",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildCosignArgs(tt.pub, tt.ref)
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildCosignArgs err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildCosignArgs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPinImageToDigest(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		digest string
		want   string
	}{
		{"tag + digest", "ghcr.io/o/r:v1", "sha256:abc", "ghcr.io/o/r@sha256:abc"},
		{"no tag + digest", "ghcr.io/o/r", "sha256:abc", "ghcr.io/o/r@sha256:abc"},
		{"registry port + tag", "registry:5000/o/r:v1", "sha256:abc", "registry:5000/o/r@sha256:abc"},
		{"already pinned", "ghcr.io/o/r@sha256:def", "sha256:abc", "ghcr.io/o/r@sha256:def"},
		{"no digest", "ghcr.io/o/r:v1", "", ""},
		{"bare name + tag", "redis:7", "sha256:abc", "redis@sha256:abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pinImageToDigest(tt.ref, tt.digest); got != tt.want {
				t.Errorf("pinImageToDigest(%q,%q) = %q, want %q", tt.ref, tt.digest, got, tt.want)
			}
		})
	}
}

func TestCosignAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH/exec semantics differ on windows")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if CosignAvailable() {
		t.Error("expected cosign unavailable with empty PATH dir")
	}
	// Drop a fake executable named cosign and confirm it is found.
	fake := filepath.Join(dir, "cosign")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if !CosignAvailable() {
		t.Error("expected cosign available after dropping a fake binary on PATH")
	}
}

func TestVerifyModule_NoOpWhenNoPublisher(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := gh("acme", "widget")
	res, err := VerifyModule(src, []LockImage{{Ref: "ghcr.io/acme/widget:v1", Digest: "sha256:abc"}})
	if err != nil {
		t.Fatalf("expected no-op nil error, got %v", err)
	}
	if !res.Skipped || res.Verified {
		t.Errorf("expected skipped no-op, got %+v", res)
	}
}

func TestVerifyModule_NoOpWhenNotRequired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Publisher present but does NOT require a signature -> still a no-op.
	if err := SetVerifiedPublisher(VerifiedPublisher{Pattern: "acme/widget", Key: "/k", RequireSignature: false}); err != nil {
		t.Fatal(err)
	}
	res, err := VerifyModule(gh("acme", "widget"), []LockImage{{Ref: "img", Digest: "sha256:abc"}})
	if err != nil {
		t.Fatalf("expected no-op nil error, got %v", err)
	}
	if !res.Skipped {
		t.Errorf("expected skipped, got %+v", res)
	}
}

func TestVerifyModule_RefusesWithoutDigest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := SetVerifiedPublisher(VerifiedPublisher{Pattern: "acme/widget", Key: "/k", RequireSignature: true}); err != nil {
		t.Fatal(err)
	}
	// Stub cosign as available + always-pass, so the refusal can only come from
	// the missing digest (not cosign absence).
	defer stubCosign(t, func(VerifiedPublisher, string) error { return nil })()
	_, err := VerifyModule(gh("acme", "widget"), []LockImage{{Ref: "img-no-digest", Digest: ""}})
	if err == nil {
		t.Fatal("expected refusal when a required signature has no resolvable digest")
	}
}

func TestVerifyModule_VerifiesWithStub(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := SetVerifiedPublisher(VerifiedPublisher{Pattern: "acme/widget", Key: "/k", RequireSignature: true}); err != nil {
		t.Fatal(err)
	}
	var got string
	defer stubCosign(t, func(_ VerifiedPublisher, pinned string) error { got = pinned; return nil })()
	res, err := VerifyModule(gh("acme", "widget"), []LockImage{{Ref: "ghcr.io/acme/widget:v1", Digest: "sha256:abc"}})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !res.Verified {
		t.Errorf("expected Verified, got %+v", res)
	}
	if got != "ghcr.io/acme/widget@sha256:abc" {
		t.Errorf("cosign verified %q, want digest-pinned ref", got)
	}
}

// stubCosign replaces both the cosign-availability check (by prepending a temp
// PATH dir containing a fake cosign) and the runCosignVerify seam, restoring
// both on cleanup.
func stubCosign(t *testing.T, fn func(VerifiedPublisher, string) error) func() {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		_ = os.WriteFile(filepath.Join(dir, "cosign"), []byte("#!/bin/sh\nexit 0\n"), 0755)
		t.Setenv("PATH", dir)
	}
	prev := runCosignVerify
	runCosignVerify = fn
	return func() { runCosignVerify = prev }
}
