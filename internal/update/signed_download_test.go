package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/releasesig"
)

// fakeReleaseServer serves GitHub-shaped release metadata and assets for
// one or more tags. Assets are keyed by "<tag>/<name>"; a missing key is 404.
type fakeReleaseServer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	assets map[string][]byte
	hits   map[string]int
	latest string
}

func newFakeReleaseServer(t *testing.T) *fakeReleaseServer {
	t.Helper()
	f := &fakeReleaseServer{t: t, assets: map[string][]byte{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeReleaseServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	f.hits[path]++
	const apiPrefix = "/repos/" + GitHubRepo + "/releases/"
	const dlPrefix = "/" + GitHubRepo + "/releases/download/"
	switch {
	case path == apiPrefix+"latest":
		_ = json.NewEncoder(w).Encode(Release{TagName: f.latest})
	case strings.HasPrefix(path, apiPrefix+"tags/"):
		_ = json.NewEncoder(w).Encode(Release{TagName: strings.TrimPrefix(path, apiPrefix+"tags/")})
	case strings.HasPrefix(path, dlPrefix):
		body, ok := f.assets[strings.TrimPrefix(path, dlPrefix)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeReleaseServer) client(current string) *Client {
	return NewClient(current).WithBaseURLs(f.srv.URL, f.srv.URL)
}

func (f *fakeReleaseServer) archiveHits(tag string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits["/"+GitHubRepo+"/releases/download/"+tag+"/"+platformArchiveName(tag)]
}

func platformArchiveName(tag string) string {
	return (&Client{}).getBinaryArchiveName(&Release{TagName: tag})
}

// testArchive builds this platform's release archive containing payload as
// the citadel binary.
func testArchive(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("citadel.exe")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(payload)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "citadel", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func checksumsFor(tag string, archive []byte) []byte {
	sum := sha256.Sum256(archive)
	return []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), platformArchiveName(tag)))
}

type testSigner struct {
	priv ed25519.PrivateKey
	keys []releasesig.TrustedKey
}

func newTestSigner(t *testing.T) testSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{priv: priv, keys: []releasesig.TrustedKey{{ID: releasesig.KeyID(pub), PublicKey: pub}}}
}

func (s testSigner) sign(t *testing.T, msg []byte) []byte {
	t.Helper()
	sig, err := releasesig.Sign(s.priv, msg)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// publish adds a complete release for tag. sig == nil publishes no signature.
func (f *fakeReleaseServer) publish(tag string, archive, checksums, sig []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets[tag+"/"+platformArchiveName(tag)] = archive
	f.assets[tag+"/checksums.txt"] = checksums
	if sig != nil {
		f.assets[tag+"/checksums.txt.sig"] = sig
	}
}

func candidatePath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "candidate")
}

func TestDownloadAndVerifySignedRoundtrip(t *testing.T) {
	f := newFakeReleaseServer(t)
	s := newTestSigner(t)
	payload := []byte("signed binary payload")
	archive := testArchive(t, payload)
	sums := checksumsFor("v2.300.0", archive)
	f.publish("v2.300.0", archive, sums, s.sign(t, sums))

	c := f.client("v2.299.0")
	c.trustedKeys = s.keys
	dest := candidatePath(t)
	if err := c.DownloadAndVerify(&Release{TagName: "v2.300.0"}, dest); err != nil {
		t.Fatalf("DownloadAndVerify: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("extracted %q (%v), want %q", got, err, payload)
	}
}

func TestDownloadAndVerifyRefusesMissingSignatureBeforeArchive(t *testing.T) {
	f := newFakeReleaseServer(t)
	archive := testArchive(t, []byte("unsigned"))
	f.publish("v2.300.0", archive, checksumsFor("v2.300.0", archive), nil)

	err := f.client("v2.299.0").DownloadAndVerify(&Release{TagName: "v2.300.0"}, candidatePath(t))
	if !errors.Is(err, ErrUnsignedRelease) {
		t.Fatalf("err = %v, want ErrUnsignedRelease", err)
	}
	if n := f.archiveHits("v2.300.0"); n != 0 {
		t.Fatalf("archive fetched %d times for an unsigned release", n)
	}
}

func TestDownloadAndVerifyRefusesTamperedChecksums(t *testing.T) {
	f := newFakeReleaseServer(t)
	s := newTestSigner(t)
	genuine := testArchive(t, []byte("genuine"))
	evil := testArchive(t, []byte("substituted"))
	sig := s.sign(t, checksumsFor("v2.300.0", genuine))
	// Archive and checksums both replaced; the old signature is kept.
	f.publish("v2.300.0", evil, checksumsFor("v2.300.0", evil), sig)

	c := f.client("v2.299.0")
	c.trustedKeys = s.keys
	err := c.DownloadAndVerify(&Release{TagName: "v2.300.0"}, candidatePath(t))
	if !errors.Is(err, ErrReleaseSignature) || !errors.Is(err, releasesig.ErrBadSignature) {
		t.Fatalf("err = %v, want ErrReleaseSignature/ErrBadSignature", err)
	}
	if n := f.archiveHits("v2.300.0"); n != 0 {
		t.Fatalf("archive fetched %d times despite a bad signature", n)
	}
}

func TestDownloadAndVerifyRefusesArchiveNotMatchingSignedDigest(t *testing.T) {
	f := newFakeReleaseServer(t)
	s := newTestSigner(t)
	genuine := testArchive(t, []byte("genuine"))
	sums := checksumsFor("v2.300.0", genuine)
	f.publish("v2.300.0", testArchive(t, []byte("substituted")), sums, s.sign(t, sums))

	c := f.client("v2.299.0")
	c.trustedKeys = s.keys
	dest := candidatePath(t)
	err := c.DownloadAndVerify(&Release{TagName: "v2.300.0"}, dest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("candidate written despite mismatch: %v", statErr)
	}
}

func TestDownloadAndVerifyRefusesUnknownKey(t *testing.T) {
	f := newFakeReleaseServer(t)
	s := newTestSigner(t)
	archive := testArchive(t, []byte("x"))
	sums := checksumsFor("v2.300.0", archive)
	f.publish("v2.300.0", archive, sums, s.sign(t, sums))

	// The default client trusts only the compiled release key set.
	err := f.client("v2.299.0").DownloadAndVerify(&Release{TagName: "v2.300.0"}, candidatePath(t))
	if !errors.Is(err, ErrReleaseSignature) || !errors.Is(err, releasesig.ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrReleaseSignature/ErrUnknownKey", err)
	}
}

func TestDownloadAndVerifyRefusesReplayedSignedChecksums(t *testing.T) {
	f := newFakeReleaseServer(t)
	s := newTestSigner(t)
	oldArchive := testArchive(t, []byte("old release"))
	oldSums := checksumsFor("v2.300.0", oldArchive)
	oldSig := s.sign(t, oldSums)
	f.publish("v2.300.0", oldArchive, oldSums, oldSig)
	// A newer tag republishing the old release's validly signed checksums and
	// signature next to an archive under the new name.
	f.mu.Lock()
	f.assets["v2.301.0/"+platformArchiveName("v2.301.0")] = oldArchive
	f.assets["v2.301.0/checksums.txt"] = oldSums
	f.assets["v2.301.0/checksums.txt.sig"] = oldSig
	f.mu.Unlock()

	c := f.client("v2.300.0")
	c.trustedKeys = s.keys
	err := c.DownloadAndVerify(&Release{TagName: "v2.301.0"}, candidatePath(t))
	if !errors.Is(err, ErrReleaseSignature) {
		t.Fatalf("err = %v, want ErrReleaseSignature", err)
	}
	if n := f.archiveHits("v2.301.0"); n != 0 {
		t.Fatalf("archive fetched %d times for a replayed checksums file", n)
	}
}

func TestDownloadAndVerifyRejectsInvalidTag(t *testing.T) {
	f := newFakeReleaseServer(t)
	for _, tag := range []string{"", "latest", "v1.2.3/../../x"} {
		err := f.client("v1.0.0").DownloadAndVerify(&Release{TagName: tag}, candidatePath(t))
		if !errors.Is(err, ErrReleaseSignature) {
			t.Errorf("tag %q: err = %v, want ErrReleaseSignature", tag, err)
		}
	}
	if err := f.client("v1.0.0").DownloadAndVerify(nil, candidatePath(t)); !errors.Is(err, ErrReleaseSignature) {
		t.Errorf("nil release: err = %v", err)
	}
}

func TestCheckForUpdateAndGetReleaseByTagUseConfiguredBase(t *testing.T) {
	f := newFakeReleaseServer(t)
	f.latest = "v2.300.0"
	rel, err := f.client("v2.299.0").CheckForUpdate()
	if err != nil || rel == nil || rel.TagName != "v2.300.0" {
		t.Fatalf("CheckForUpdate = %+v, %v", rel, err)
	}
	rel, err = f.client("v2.299.0").GetReleaseByTag("2.300.0")
	if err != nil || rel.TagName != "v2.300.0" {
		t.Fatalf("GetReleaseByTag = %+v, %v", rel, err)
	}
	if _, err := f.client("v2.299.0").GetReleaseByTag("v1.0.0/../../x"); err == nil {
		t.Fatal("GetReleaseByTag accepted a path-shaped tag")
	}
}

// TestAutoUpdaterRefusesUnsignedRelease drives the periodic updater with the
// real Client against a fake release server publishing an unsigned release.
func TestAutoUpdaterRefusesUnsignedRelease(t *testing.T) {
	isolateState(t)
	f := newFakeReleaseServer(t)
	f.latest = "v9.9.9"
	archive := testArchive(t, []byte("unsigned"))
	f.publish("v9.9.9", archive, checksumsFor("v9.9.9", archive), nil)

	applied, drained := false, false
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker:    f.client("v1.0.0"),
		Apply:      func(string) error { applied = true; return nil },
		Restart:    func() error { return nil },
		BeginDrain: func() func() { drained = true; return func() {} },
	})
	if u.runOnce(context.Background()) {
		t.Fatal("runOnce reported a restart for an unsigned release")
	}
	if applied || drained {
		t.Fatalf("unsigned release reached apply=%v drain=%v", applied, drained)
	}
	if n := f.archiveHits("v9.9.9"); n != 0 {
		t.Fatalf("archive fetched %d times for an unsigned release", n)
	}
	assertResult(t, ResultSignatureMissing)
}

func TestDownloadFailureResultClassifiesSignatureRefusals(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"missing":   {fmt.Errorf("v1.2.3: %w", ErrUnsignedRelease), ResultSignatureMissing},
		"bad sig":   {fmt.Errorf("%w: v1.2.3: %w", ErrReleaseSignature, releasesig.ErrBadSignature), ResultSignatureInvalid},
		"unknown":   {fmt.Errorf("%w: v1.2.3: %w", ErrReleaseSignature, releasesig.ErrUnknownKey), ResultSignatureInvalid},
		"transient": {errors.New("connection reset"), ResultDownloadFailed},
		"mismatch":  {errors.New("checksum mismatch: expected a, got b"), ResultDownloadFailed},
	}
	for name, tc := range cases {
		if got := DownloadFailureResult(tc.err); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
		if got := IsReleaseSignatureError(tc.err); got != (tc.want != ResultDownloadFailed) {
			t.Errorf("%s: IsReleaseSignatureError = %v", name, got)
		}
	}
}

// TestAutoUpdaterRecordsSignatureInvalid covers a present-but-bad signature
// (here, a signed checksums file replayed under a newer tag).
func TestAutoUpdaterRecordsSignatureInvalid(t *testing.T) {
	isolateState(t)
	f := newFakeReleaseServer(t)
	f.latest = "v9.9.9"
	s := newTestSigner(t)
	archive := testArchive(t, []byte("old"))
	oldSums := checksumsFor("v9.9.8", archive)
	f.mu.Lock()
	f.assets["v9.9.9/"+platformArchiveName("v9.9.9")] = archive
	f.assets["v9.9.9/checksums.txt"] = oldSums
	f.assets["v9.9.9/checksums.txt.sig"] = s.sign(t, oldSums)
	f.mu.Unlock()

	c := f.client("v1.0.0")
	c.trustedKeys = s.keys
	applied := false
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker:    c,
		Apply:      func(string) error { applied = true; return nil },
		Restart:    func() error { return nil },
		BeginDrain: func() func() { return func() {} },
	})
	if u.runOnce(context.Background()) || applied {
		t.Fatal("a replayed signed checksums file reached apply")
	}
	assertResult(t, ResultSignatureInvalid)
}

func TestDownloadAndVerifyBoundsArchiveSize(t *testing.T) {
	old := maxArchiveSize
	maxArchiveSize = 1024
	t.Cleanup(func() { maxArchiveSize = old })

	big := bytes.Repeat([]byte("x"), 4096)
	sum := sha256.Sum256(big)
	sums := []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), platformArchiveName("v2.300.0")))
	s := newTestSigner(t)

	// Advertised Content-Length over the cap.
	f := newFakeReleaseServer(t)
	f.publish("v2.300.0", big, sums, s.sign(t, sums))
	c := f.client("v2.299.0")
	c.trustedKeys = s.keys
	dest := candidatePath(t)
	if err := c.DownloadAndVerify(&Release{TagName: "v2.300.0"}, dest); !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("content-length: err = %v, want ErrArchiveTooLarge", err)
	}

	// No Content-Length (streamed): the copy itself must stop at the cap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write(sums)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt.sig"):
			_, _ = w.Write(s.sign(t, sums))
		default:
			fl := w.(http.Flusher)
			for i := 0; i < 64; i++ {
				_, _ = w.Write(big)
				fl.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	c = NewClient("v2.299.0").WithBaseURLs(srv.URL, srv.URL)
	c.trustedKeys = s.keys
	dest = candidatePath(t)
	if err := c.DownloadAndVerify(&Release{TagName: "v2.300.0"}, dest); !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("streamed: err = %v, want ErrArchiveTooLarge", err)
	}
	if _, err := os.Stat(dest + ".archive"); !os.IsNotExist(err) {
		t.Fatalf("oversized partial archive left behind: %v", err)
	}
}

func TestDownloadAndVerifyPlatformNotPublishedIsTerminal(t *testing.T) {
	f := newFakeReleaseServer(t)
	s := newTestSigner(t)
	other := "other_os"
	if runtime.GOOS == other {
		t.Skip()
	}
	sums := []byte(strings.Repeat("a", 64) + "  citadel_v2.300.0_" + other + "_amd64.tar.gz\n")
	f.publish("v2.300.0", []byte("unused"), sums, s.sign(t, sums))
	c := f.client("v2.299.0")
	c.trustedKeys = s.keys
	err := c.DownloadAndVerify(&Release{TagName: "v2.300.0"}, candidatePath(t))
	if !errors.Is(err, ErrPlatformNotPublished) || !IsTerminalReleaseError(err) || IsReleaseSignatureError(err) {
		t.Fatalf("err = %v, want terminal ErrPlatformNotPublished", err)
	}
	if got := DownloadFailureResult(err); got != ResultPlatformNotPublished {
		t.Fatalf("result = %s, want %s", got, ResultPlatformNotPublished)
	}
	if n := f.archiveHits("v2.300.0"); n != 0 {
		t.Fatalf("archive fetched %d times", n)
	}
	if IsTerminalReleaseError(fmt.Errorf("x: %w", ErrArchiveTooLarge)) || IsTerminalReleaseError(errors.New("connection reset")) {
		t.Fatal("transient failures must stay retryable")
	}
}
