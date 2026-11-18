//go:build !windows

package botprofile

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var (
	idA = Identity{OrgID: "org-1", IdentityID: "ident-a"}
	idB = Identity{OrgID: "org-1", IdentityID: "ident-b"}
)

func share(b byte) []byte { return bytes.Repeat([]byte{b}, ShareLen) }

func providerFor(ids ...Identity) StaticShares {
	m := StaticShares{}
	for i, id := range ids {
		m[id.OrgID+"/"+id.IdentityID] = share(byte(0x10 + i))
	}
	return m
}

// seal opens, writes one file, persists and closes.
func seal(t *testing.T, dir string, id Identity, sp ShareProvider, major int, content string) {
	t.Helper()
	h, err := Open(context.Background(), dir, id, sp, major)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer h.Close()
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "Cookies"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Persist(work); err != nil {
		t.Fatalf("persist: %v", err)
	}
}

func materialize(t *testing.T, dir string, id Identity, sp ShareProvider, major int) (string, error) {
	t.Helper()
	h, err := Open(context.Background(), dir, id, sp, major)
	if err != nil {
		return "", err
	}
	defer h.Close()
	work := t.TempDir()
	if err := h.Materialize(work); err != nil {
		return "", err
	}
	b, _ := os.ReadFile(filepath.Join(work, "Cookies"))
	return string(b), nil
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sp := providerFor(idA)
	seal(t, dir, idA, sp, 120, "secret-cookie-jar")
	blob, err := os.ReadFile(filepath.Join(Dir(dir), idA.ProfileName()+".enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("secret-cookie-jar")) {
		t.Fatal("plaintext present in sealed blob")
	}
	got, err := materialize(t, dir, idA, sp, 120)
	if err != nil || got != "secret-cookie-jar" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestWrongNodeShareFails(t *testing.T) {
	dir := t.TempDir()
	sp := providerFor(idA)
	seal(t, dir, idA, sp, 120, "x")
	p := filepath.Join(Dir(dir), NodeShareFile)
	if err := os.WriteFile(p, share(0xEE), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materialize(t, dir, idA, sp, 120); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("want ErrOpenFailed, got %v", err)
	}
}

func TestWrongAceTeamShareFails(t *testing.T) {
	dir := t.TempDir()
	seal(t, dir, idA, providerFor(idA), 120, "x")
	wrong := StaticShares{idA.OrgID + "/" + idA.IdentityID: share(0xEE)}
	if _, err := materialize(t, dir, idA, wrong, 120); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("want ErrOpenFailed, got %v", err)
	}
}

func TestSwappedIdentityBlobFails(t *testing.T) {
	dir := t.TempDir()
	// Same shares for both rows, so only the identity binding can stop the swap.
	sp := StaticShares{
		idA.OrgID + "/" + idA.IdentityID: share(0x42),
		idB.OrgID + "/" + idB.IdentityID: share(0x42),
	}
	seal(t, dir, idA, sp, 120, "a-cookies")
	seal(t, dir, idB, sp, 120, "b-cookies")
	a := filepath.Join(Dir(dir), idA.ProfileName()+".enc")
	b := filepath.Join(Dir(dir), idB.ProfileName()+".enc")
	blobA, _ := os.ReadFile(a)
	if err := os.WriteFile(b, blobA, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materialize(t, dir, idB, sp, 120); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("swapped identity blob opened: %v", err)
	}
}

func TestContextBindsOrgAndIdentityEvenWithSameKey(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	mk := func(id Identity) *sealer {
		s, err := newSealer(key, id, 120)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base := Identity{OrgID: "org-1", IdentityID: "ident-a"}
	ct, err := mk(base).Seal(contextLabel(base), []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]Identity{
		"org":      {OrgID: "org-2", IdentityID: "ident-a"},
		"identity": {OrgID: "org-1", IdentityID: "ident-b"},
	} {
		if _, err := mk(other).Unseal(contextLabel(base), ct); !errors.Is(err, ErrOpenFailed) {
			t.Fatalf("%s swap opened: %v", name, err)
		}
	}
	if _, err := mk(base).Unseal(contextLabel(idB), ct); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("label swap opened: %v", err)
	}
	if _, err := mk(base).Unseal(contextLabel(base), ct); err != nil {
		t.Fatalf("control open failed: %v", err)
	}
}

func TestSwappedOrgFails(t *testing.T) {
	dir := t.TempDir()
	org2 := Identity{OrgID: "org-2", IdentityID: idA.IdentityID}
	sp := StaticShares{
		idA.OrgID + "/" + idA.IdentityID:   share(0x42),
		org2.OrgID + "/" + org2.IdentityID: share(0x42),
	}
	seal(t, dir, idA, sp, 120, "x")
	if _, err := materialize(t, dir, org2, sp, 120); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("cross-org open: %v", err)
	}
}

func TestChromiumVersionGuard(t *testing.T) {
	dir := t.TempDir()
	sp := providerFor(idA)
	seal(t, dir, idA, sp, 120, "x")
	if _, err := materialize(t, dir, idA, sp, 119); !errors.Is(err, ErrNewerChromium) {
		t.Fatalf("older runtime: want ErrNewerChromium, got %v", err)
	}
	for _, major := range []int{120, 121} {
		if got, err := materialize(t, dir, idA, sp, major); err != nil || got != "x" {
			t.Fatalf("major %d: %q %v", major, got, err)
		}
	}
	if _, err := Open(context.Background(), dir, idA, sp, 0); !errors.Is(err, ErrUnknownChromium) {
		t.Fatalf("unknown runtime major: %v", err)
	}
	// The recorded major follows the runtime that last persisted.
	seal(t, dir, idA, sp, 125, "y")
	if _, err := materialize(t, dir, idA, sp, 120); !errors.Is(err, ErrNewerChromium) {
		t.Fatalf("after upgrade: %v", err)
	}
}

func TestParseChromiumMajor(t *testing.T) {
	for in, want := range map[string]int{"120.0.6099.109": 120, " 99 ": 99} {
		if got, err := ParseChromiumMajor(in); err != nil || got != want {
			t.Fatalf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x.1", "0.1", "-3.0", "99999.0"} {
		if _, err := ParseChromiumMajor(in); err == nil {
			t.Fatalf("%q accepted", in)
		}
	}
}

func TestKDFAndAADConstructionPinned(t *testing.T) {
	ns, as := share(1), share(2)
	got, err := deriveKey(ns, as, idA)
	if err != nil {
		t.Fatal(err)
	}
	info := "citadel-cli/bot-profile/key/v1\x00" + "\x05org-1" + "\x07ident-a"
	want, err := hkdf.Key(sha256.New, append(append([]byte{}, ns...), as...),
		[]byte("citadel-cli/bot-profile/hkdf-salt/v1"), info, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("KDF construction changed")
	}
	other, _ := deriveKey(ns, as, idB)
	if bytes.Equal(got, other) {
		t.Fatal("key not bound to identity")
	}
	if _, err := deriveKey(ns[:31], as, idA); !errors.Is(err, ErrBadShare) {
		t.Fatalf("short node share: %v", err)
	}
	if _, err := deriveKey(ns, append(as, 0), idA); !errors.Is(err, ErrBadShare) {
		t.Fatalf("long ace share: %v", err)
	}
}

func TestNonceFreshPerSeal(t *testing.T) {
	s, _ := newSealer(bytes.Repeat([]byte{7}, 32), idA, 120)
	a, _ := s.Seal("l", []byte("p"))
	b, _ := s.Seal("l", []byte("p"))
	if bytes.Equal(a[1:13], b[1:13]) {
		t.Fatal("nonce reused")
	}
	if a[0] != 1 {
		t.Fatal("blob version byte")
	}
}

func TestSealerLockedAfterClose(t *testing.T) {
	s, _ := newSealer(bytes.Repeat([]byte{7}, 32), idA, 120)
	s.Lock()
	if _, err := s.Seal("l", nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("seal after lock: %v", err)
	}
}

func TestNodeShareFilePermissionsAndOwner(t *testing.T) {
	dir := t.TempDir()
	seal(t, dir, idA, providerFor(idA), 120, "x")
	sdir := Dir(dir)
	var ds, fs unix.Stat_t
	if err := unix.Lstat(sdir, &ds); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lstat(filepath.Join(sdir, NodeShareFile), &fs); err != nil {
		t.Fatal(err)
	}
	if ds.Mode&0o777 != 0o700 {
		t.Fatalf("dir mode %o", ds.Mode&0o777)
	}
	if fs.Mode&0o777 != 0o600 || fs.Size != ShareLen {
		t.Fatalf("file mode %o size %d", fs.Mode&0o777, fs.Size)
	}
	if int(ds.Uid) != os.Geteuid() || int(fs.Uid) != os.Geteuid() {
		t.Fatal("owner is not the running user")
	}
	// Stable across opens.
	b1, _ := os.ReadFile(filepath.Join(sdir, NodeShareFile))
	_, _ = materialize(t, dir, idA, providerFor(idA), 120)
	b2, _ := os.ReadFile(filepath.Join(sdir, NodeShareFile))
	if !bytes.Equal(b1, b2) {
		t.Fatal("node share changed")
	}
}

func TestNodeShareRefusals(t *testing.T) {
	sp := providerFor(idA)
	open := func(dir string) error {
		_, err := materialize(t, dir, idA, sp, 120)
		return err
	}
	t.Run("file group-readable", func(t *testing.T) {
		dir := t.TempDir()
		seal(t, dir, idA, sp, 120, "x")
		p := filepath.Join(Dir(dir), NodeShareFile)
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := open(dir); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("want refusal, got %v", err)
		}
	})
	t.Run("dir world-accessible", func(t *testing.T) {
		dir := t.TempDir()
		seal(t, dir, idA, sp, 120, "x")
		if err := os.Chmod(Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := open(dir); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("want refusal, got %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		seal(t, dir, idA, sp, 120, "x")
		p := filepath.Join(Dir(dir), NodeShareFile)
		real := filepath.Join(dir, "elsewhere")
		if err := os.Rename(p, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, p); err != nil {
			t.Fatal(err)
		}
		if err := open(dir); err == nil {
			t.Fatal("symlinked node share accepted")
		}
	})
	t.Run("wrong length", func(t *testing.T) {
		dir := t.TempDir()
		seal(t, dir, idA, sp, 120, "x")
		p := filepath.Join(Dir(dir), NodeShareFile)
		if err := os.WriteFile(p, share(1)[:16], 0o600); err != nil {
			t.Fatal(err)
		}
		if err := open(dir); err == nil {
			t.Fatal("short node share accepted")
		}
	})
	t.Run("other owner", func(t *testing.T) {
		st := unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: uint32(os.Geteuid()) + 1}
		if err := checkOwnerMode(&st, unix.S_IFREG, "file"); err == nil {
			t.Fatal("foreign owner accepted")
		}
	})
}

// Sentinel: no error string from any failure path contains a share, the
// derived key, or a provider's own error text, in raw, hex or base64 form.
type leakyProvider struct{ share []byte }

func (p leakyProvider) AceTeamShare(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("backend said " + string(p.share))
}

func TestNoKeyMaterialInErrors(t *testing.T) {
	sentinel := bytes.Repeat([]byte("SENTINEL"), 4) // 32 bytes
	dir := t.TempDir()
	good := StaticShares{idA.OrgID + "/" + idA.IdentityID: sentinel}
	seal(t, dir, idA, good, 120, "x")
	nodeShare, err := os.ReadFile(filepath.Join(Dir(dir), NodeShareFile))
	if err != nil {
		t.Fatal(err)
	}
	derived, _ := deriveKey(nodeShare, sentinel, idA)

	var errs []error
	collect := func(_ string, err error) { errs = append(errs, err) }
	collect(materialize(t, dir, idA, StaticShares{idA.OrgID + "/" + idA.IdentityID: share(9)}, 120))
	collect(materialize(t, dir, idA, good, 119))
	collect(materialize(t, dir, idA, leakyProvider{sentinel}, 120))
	collect(materialize(t, dir, idA, StaticShares{idA.OrgID + "/" + idA.IdentityID: sentinel[:31]}, 120))
	collect(materialize(t, dir, Identity{OrgID: "org-1", IdentityID: "BAD ID"}, good, 120))
	_ = os.Chmod(filepath.Join(Dir(dir), NodeShareFile), 0o644)
	collect(materialize(t, dir, idA, good, 120))

	needles := [][]byte{sentinel, nodeShare, derived}
	var forms []string
	for _, n := range needles {
		forms = append(forms, string(n), hex.EncodeToString(n),
			base64.StdEncoding.EncodeToString(n), base64.RawURLEncoding.EncodeToString(n))
	}
	seen := 0
	for _, err := range errs {
		if err == nil {
			t.Fatal("expected a failure")
		}
		seen++
		for _, f := range forms {
			if strings.Contains(err.Error(), f) {
				t.Fatalf("error leaks key material: %v", err)
			}
		}
	}
	if seen != 6 {
		t.Fatalf("seen %d", seen)
	}
}

func TestResetRemovesBlobOnly(t *testing.T) {
	dir := t.TempDir()
	sp := providerFor(idA)
	seal(t, dir, idA, sp, 120, "x")
	if err := Reset(dir, idA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(dir), idA.ProfileName()+".enc")); !os.IsNotExist(err) {
		t.Fatal("blob still present")
	}
	if got, err := materialize(t, dir, idA, sp, 120); err != nil || got != "" {
		t.Fatalf("fresh profile after reset: %q %v", got, err)
	}
}

func TestFlippedVersionByteRefused(t *testing.T) {
	dir := t.TempDir()
	sp := providerFor(idA)
	seal(t, dir, idA, sp, 120, "x")
	p := filepath.Join(Dir(dir), idA.ProfileName()+".enc")
	blob, _ := os.ReadFile(p)
	blob[0] ^= 0x01
	if err := os.WriteFile(p, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materialize(t, dir, idA, sp, 120); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("flipped version byte accepted: %v", err)
	}
	blob[0] ^= 0x01
	blob[len(blob)-1] ^= 0x01
	_ = os.WriteFile(p, blob, 0o600)
	if _, err := materialize(t, dir, idA, sp, 120); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("tampered ciphertext accepted: %v", err)
	}
}

func TestOversizedNodeShareRefused(t *testing.T) {
	for _, n := range []int{ShareLen + 1, 1 << 20} {
		dir := t.TempDir()
		sp := providerFor(idA)
		seal(t, dir, idA, sp, 120, "x")
		p := filepath.Join(Dir(dir), NodeShareFile)
		if err := os.WriteFile(p, bytes.Repeat([]byte{1}, n), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := materialize(t, dir, idA, sp, 120); err == nil {
			t.Fatalf("share of %d bytes accepted", n)
		}
	}
}

func TestNodeShareExtraHardLinkRefused(t *testing.T) {
	dir := t.TempDir()
	sp := providerFor(idA)
	seal(t, dir, idA, sp, 120, "x")
	if err := os.Link(filepath.Join(Dir(dir), NodeShareFile), filepath.Join(dir, "second-name")); err != nil {
		t.Skip("hard links unavailable")
	}
	if _, err := materialize(t, dir, idA, sp, 120); err == nil {
		t.Fatal("multiply linked share accepted")
	}
}

func TestPlantedShareIsNotAdopted(t *testing.T) {
	dir := t.TempDir()
	sdir := Dir(dir)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(sdir, NodeShareFile)
	if err := os.WriteFile(planted, share(0x66), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createShare(sdir, planted); !errors.Is(err, ErrNodeShareContested) {
		t.Fatalf("want ErrNodeShareContested, got %v", err)
	}
	if b, _ := os.ReadFile(planted); !bytes.Equal(b, share(0x66)) {
		t.Fatal("existing share was overwritten")
	}
}

func TestStaleShareTempsRemovedFreshKept(t *testing.T) {
	dir := t.TempDir()
	sdir := Dir(dir)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(sdir, ".share-old")
	fresh := filepath.Join(sdir, ".share-new")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	seal(t, dir, idA, providerFor(idA), 120, "x")
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatal("stale temp kept")
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Fatal("fresh temp removed")
	}
}
