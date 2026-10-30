package containerjobs

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testOrg = "00000000-0000-4000-8000-000000000001"
const otherOrg = "00000000-0000-4000-8000-000000000002"
const testTransfer = "00000000-0000-4000-8000-000000000003"
const testContainer = "sandbox-1234abcd"
const testVolume = "aceteam-vol-1234abcd"
const newVolume = "aceteam-vol-5678abcd"

type fakeRuntime struct {
	root       string
	org        string
	running    bool
	volumes    map[string]string
	mu         sync.Mutex
	effects    []string
	execResult ExecResult
	afterLock  func()
}

func newFake(t *testing.T) *fakeRuntime {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, testVolume)
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture")
	}
	return &fakeRuntime{root: root, org: testOrg, volumes: map[string]string{testVolume: testOrg}}
}
func (f *fakeRuntime) Lock(ctx context.Context) (func(), error) {
	f.mu.Lock()
	if f.afterLock != nil {
		f.afterLock()
	}
	return f.mu.Unlock, nil
}
func (f *fakeRuntime) Container(context.Context, string) (ContainerState, error) {
	return ContainerState{OrgID: f.org, VolumeRef: testVolume, Running: f.running}, nil
}
func (f *fakeRuntime) Volume(_ context.Context, ref string) (VolumeState, error) {
	org, ok := f.volumes[ref]
	if !ok {
		return VolumeState{}, ErrNotFound
	}
	return VolumeState{OrgID: org, QuotaBytes: 16 << 20}, nil
}
func (f *fakeRuntime) CreateVolume(_ context.Context, r Request) error {
	f.effects = append(f.effects, "create-volume")
	if _, ok := f.volumes[r.VolumeRef]; ok {
		return ErrUnavailable
	}
	if err := os.Mkdir(filepath.Join(f.root, r.VolumeRef), 0700); err != nil {
		return err
	}
	f.volumes[r.VolumeRef] = r.OrgID
	return nil
}
func (f *fakeRuntime) CreateContainer(context.Context, Request) error {
	f.effects = append(f.effects, "create-container")
	return nil
}
func (f *fakeRuntime) Start(context.Context, string) error {
	f.effects = append(f.effects, "start")
	f.running = true
	return nil
}
func (f *fakeRuntime) Stop(context.Context, string) error {
	f.effects = append(f.effects, "stop")
	f.running = false
	return nil
}
func (f *fakeRuntime) Exec(context.Context, string, []string) (ExecResult, error) {
	f.effects = append(f.effects, "exec")
	return f.execResult, nil
}
func (f *fakeRuntime) OpenVolume(_ context.Context, ref string) (*os.Root, error) {
	f.effects = append(f.effects, "open")
	return os.OpenRoot(filepath.Join(f.root, ref))
}
func (f *fakeRuntime) Usage(context.Context, string) (int64, int64, error) {
	return 4096, 16 << 20, nil
}
func (f *fakeRuntime) DestroyContainer(context.Context, string) error {
	f.effects = append(f.effects, "destroy-container")
	return nil
}
func (f *fakeRuntime) DestroyVolume(_ context.Context, ref string) error {
	f.effects = append(f.effects, "destroy-volume")
	delete(f.volumes, ref)
	return nil
}

type fakeTransport struct{ objects map[string][]byte }

func (f *fakeTransport) Put(_ context.Context, url string, b []byte) error {
	f.objects[url] = bytes.Clone(b)
	return nil
}
func (f *fakeTransport) Get(_ context.Context, url string, limit int64) ([]byte, error) {
	b := f.objects[url]
	if int64(len(b)) > limit {
		return nil, ErrBounds
	}
	return bytes.Clone(b), nil
}
func baseRequest() Request {
	return Request{OrgID: testOrg, ContainerRef: testContainer, VolumeRef: testVolume}
}
func transferRequest() Request {
	r := baseRequest()
	r.TransferID = testTransfer
	r.KeyB64 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	r.ChunkURLs = []string{"https://blob.example.invalid/synthetic"}
	return r
}

func TestContainerEveryOperationRefusesForeignLabels(t *testing.T) {
	for _, kind := range []string{Start, Stop, Exec, FileList, FileRead, FileWrite, Usage, Export, Destroy} {
		for _, foreignVolume := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/volume", false: "/container"}[foreignVolume], func(t *testing.T) {
				f := newFake(t)
				r := baseRequest()
				r.PurgeVolume = true
				r.Path = "file"
				r.TimeoutSeconds = 1
				r.Argv = []string{"true"}
				if foreignVolume {
					f.volumes[testVolume] = otherOrg
				} else {
					f.org = otherOrg
				}
				_, err := (&Engine{Runtime: f}).Run(context.Background(), kind, r)
				if !errors.Is(err, ErrOwnership) || len(f.effects) != 0 {
					t.Fatal("foreign labels reached an effect", err, f.effects)
				}
			})
		}
	}
}
func TestContainerVolumeBindingAndPostLockPermission(t *testing.T) {
	f := newFake(t)
	r := baseRequest()
	r.VolumeRef = newVolume
	if _, err := (&Engine{Runtime: f}).Run(context.Background(), Usage, r); !errors.Is(err, ErrOwnership) {
		t.Fatal("volume binding ignored", err)
	}
	allowed := true
	f.afterLock = func() { allowed = false }
	if _, err := (&Engine{Runtime: f, Authorize: func() bool { return allowed }}).Run(context.Background(), Stop, baseRequest()); !errors.Is(err, ErrOwnership) || len(f.effects) != 0 {
		t.Fatal("revoked gate ignored after lock")
	}
}
func TestContainerStoppedFilesAndExplicitPurge(t *testing.T) {
	f := newFake(t)
	e := &Engine{Runtime: f}
	r := baseRequest()
	r.Path = "note.txt"
	r.ContentB64 = base64.StdEncoding.EncodeToString([]byte("file bytes"))
	if out, err := e.Run(context.Background(), FileWrite, r); err != nil || out.Bytes != 10 {
		t.Fatal(out, err)
	}
	if out, err := e.Run(context.Background(), FileRead, r); err != nil || out.ContentB64 != r.ContentB64 {
		t.Fatal("stopped file read failed", out, err)
	}
	r.Path = "."
	out, err := e.Run(context.Background(), FileList, r)
	if err != nil || len(out.Entries) != 1 || out.Entries[0].Name != "note.txt" {
		t.Fatal(out, err)
	}
	f.effects = nil
	if _, err = e.Run(context.Background(), Destroy, r); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.effects, ",") != "destroy-container" || f.volumes[testVolume] != testOrg {
		t.Fatal("default destroy purged disk")
	}
	r.PurgeVolume = true
	f.effects = nil
	if _, err = e.Run(context.Background(), Destroy, r); err != nil || strings.Join(f.effects, ",") != "destroy-container,destroy-volume" {
		t.Fatal("explicit purge not honored", err, f.effects)
	}
}
func TestContainerRootConfinementPreservesOutsideSentinel(t *testing.T) {
	f := newFake(t)
	outside := filepath.Join(t.TempDir(), "sentinel")
	if os.WriteFile(outside, []byte("keep"), 0600) != nil {
		t.Fatal("fixture")
	}
	if err := os.Symlink(outside, filepath.Join(f.root, testVolume, "escape")); err != nil {
		t.Skip("symlinks unavailable")
	}
	r := baseRequest()
	r.Path = "escape"
	r.ContentB64 = base64.StdEncoding.EncodeToString([]byte("overwrite"))
	for _, kind := range []string{FileRead, FileWrite} {
		if _, err := (&Engine{Runtime: f}).Run(context.Background(), kind, r); err == nil {
			t.Fatal("symlink escaped volume")
		}
	}
	b, _ := os.ReadFile(outside)
	if string(b) != "keep" {
		t.Fatal("outside bytes changed")
	}
	if os.Mkdir(filepath.Join(f.root, testVolume, "a"), 0700) != nil || os.WriteFile(filepath.Join(f.root, testVolume, "b"), []byte("keep"), 0600) != nil {
		t.Fatal("fixture")
	}
	for _, p := range []string{"../sentinel", "/etc/passwd", "a/../b", `a\b`} {
		r.Path = p
		if _, err := (&Engine{Runtime: f}).Run(context.Background(), FileWrite, r); err == nil {
			t.Fatal("path escaped", p)
		}
	}
}
func TestContainerReadAndExecOutputBounds(t *testing.T) {
	f := newFake(t)
	path := filepath.Join(f.root, testVolume, "large")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.Truncate(FileLimit+1) != nil {
		t.Fatal("fixture")
	}
	file.Close()
	r := baseRequest()
	r.Path = "large"
	if _, err := (&Engine{Runtime: f}).Run(context.Background(), FileRead, r); !errors.Is(err, ErrBounds) {
		t.Fatal("oversized read accepted", err)
	}
	f.execResult = ExecResult{Output: make([]byte, OutputLimit+1)}
	r.TimeoutSeconds = 1
	r.Argv = []string{"fixture"}
	if _, err := (&Engine{Runtime: f}).Run(context.Background(), Exec, r); !errors.Is(err, ErrBounds) {
		t.Fatal("oversized exec output accepted", err)
	}
	f.execResult = ExecResult{TimedOut: true, ExitCode: 124}
	out, err := (&Engine{Runtime: f}).Run(context.Background(), Exec, r)
	if err != nil || !out.ExecTimedOut || out.Running == nil || *out.Running || out.ExitCode != 124 || out.Message != "exec timed out; container stopped, files kept, it restarts on next use" {
		t.Fatal(out, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("timeout deleted files")
	}
}
func TestContainerEncryptedRoundTripAndFreshNonces(t *testing.T) {
	f := newFake(t)
	dir := filepath.Join(f.root, testVolume)
	want := []byte{0, 1, 2, 255, 13, 10}
	if os.WriteFile(filepath.Join(dir, "binary"), want, 0600) != nil {
		t.Fatal("fixture")
	}
	tr := &fakeTransport{objects: map[string][]byte{}}
	e := &Engine{Runtime: f, Transport: tr}
	r := transferRequest()
	exported, err := e.Run(context.Background(), Export, r)
	if err != nil {
		t.Fatal(err)
	}
	first := bytes.Clone(tr.objects[r.ChunkURLs[0]])
	again, err := e.Run(context.Background(), Export, r)
	if err != nil || bytes.Equal(first, tr.objects[r.ChunkURLs[0]]) || exported.ManifestSHA256 == again.ManifestSHA256 {
		t.Fatal("nonce reused", err)
	}
	r.VolumeRef = newVolume
	r.ContainerRef = ""
	r.QuotaBytes = 16 << 20
	r.Manifest = again.Manifest
	r.ManifestSHA256 = again.ManifestSHA256
	imported, err := e.Run(context.Background(), Import, r)
	if err != nil || imported.Bytes != again.Bytes || imported.ManifestSHA256 != again.ManifestSHA256 {
		t.Fatal(imported, err)
	}
	got, err := os.ReadFile(filepath.Join(f.root, newVolume, "binary"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("round trip lost bytes", err)
	}
}
func sealedTar(t *testing.T, r Request, name string) ([]byte, *Manifest) {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 1}) != nil {
		t.Fatal("fixture")
	}
	tw.Write([]byte("x"))
	tw.Close()
	g, _ := transferCipher(r)
	nonce := bytes.Repeat([]byte{0x11}, 12)
	sealed := g.Seal(bytes.Clone(nonce), nonce, b.Bytes(), chunkAAD(r.OrgID, r.TransferID, 0))
	digest := sha256.Sum256(sealed)
	return sealed, &Manifest{Version: 1, OrgID: r.OrgID, TransferID: r.TransferID, Chunks: []Chunk{{Index: 0, PlaintextBytes: int64(b.Len()), SHA256: hex.EncodeToString(digest[:])}}}
}
func TestContainerImportRefusesTraversalDigestAndGCMIndependently(t *testing.T) {
	for _, mode := range []string{"traversal", "digest", "tag", "manifest"} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t)
			r := transferRequest()
			r.VolumeRef = newVolume
			r.ContainerRef = ""
			r.QuotaBytes = 16 << 20
			name := "safe"
			if mode == "traversal" {
				name = "../outside"
			}
			b, m := sealedTar(t, r, name)
			if mode == "digest" {
				m.Chunks[0].SHA256 = strings.Repeat("0", 64)
			}
			if mode == "tag" {
				b[len(b)-1] ^= 1
				d := sha256.Sum256(b)
				m.Chunks[0].SHA256 = hex.EncodeToString(d[:])
			}
			r.Manifest = m
			r.ManifestSHA256 = manifestDigest(m)
			if mode == "manifest" {
				r.ManifestSHA256 = strings.Repeat("0", 64)
			}
			_, err := (&Engine{Runtime: f, Transport: &fakeTransport{objects: map[string][]byte{r.ChunkURLs[0]: b}}}).Run(context.Background(), Import, r)
			if err == nil {
				t.Fatal("unsafe import succeeded")
			}
			if _, err = os.Stat(filepath.Join(f.root, "outside")); !os.IsNotExist(err) {
				t.Fatal("import escaped root")
			}
		})
	}
}
func TestContainerExportRequiresStoppedContainer(t *testing.T) {
	f := newFake(t)
	f.running = true
	if _, err := (&Engine{Runtime: f, Transport: &fakeTransport{objects: map[string][]byte{}}}).Run(context.Background(), Export, transferRequest()); err == nil || len(f.effects) != 0 {
		t.Fatal("live container exported")
	}
}

func TestContainerProtocolBoundsAndCanonicalFields(t *testing.T) {
	good := map[string]any{"org_id": testOrg, "container_ref": testContainer, "volume_ref": testVolume, "image": "ghcr.io/aceteam-ai/workspace:v1", "quota_bytes": 16 << 20, "cpus": 1, "memory_bytes": 2 << 30, "pids_limit": 256, "argv": []string{"python", "a;echo fixture"}, "timeout_seconds": 600, "path": "note", "content_b64": "eA=="}
	for _, kind := range []string{Create, Start, Stop, Exec, FileList, FileRead, FileWrite, Usage, Destroy} {
		b, _ := json.Marshal(good)
		if _, err := Parse(kind, b); err != nil {
			t.Fatal(kind, err)
		}
	}
	for _, tc := range []struct {
		kind, key string
		value     any
	}{
		{Create, "org_id", strings.ToUpper(testOrg[:1]) + "BAD"}, {Create, "container_ref", "sandbox-../../x"}, {Create, "volume_ref", "other"}, {Create, "image", "ghcr.io/foreign/x"}, {Create, "quota_bytes", (16 << 20) + 1}, {Create, "quota_bytes", 4096}, {Create, "cpus", 0}, {Create, "memory_bytes", 0}, {Create, "pids_limit", -1}, {Create, "network", "bridge"},
		{Exec, "argv", "echo shell"}, {Exec, "argv", []string{}}, {Exec, "argv", []string{"a\x00b"}}, {Exec, "timeout_seconds", 601}, {Exec, "timeout_seconds", 0}, {Exec, "timeout_seconds", 1.5},
		{FileWrite, "path", "../outside"}, {FileRead, "path", "."}, {FileWrite, "content_b64", "eB=="}, {FileWrite, "content_b64", "not-base64"},
	} {
		c := map[string]any{}
		for k, v := range good {
			c[k] = v
		}
		c[tc.key] = tc.value
		b, _ := json.Marshal(c)
		if _, err := Parse(tc.kind, b); err == nil {
			t.Fatal("accepted invalid", tc.kind, tc.key)
		}
	}
	r := transferRequest()
	b, _ := json.Marshal(r)
	if _, err := Parse(Export, b); err != nil {
		t.Fatal(err)
	}
	r.ChunkURLs = make([]string, MaxChunks+1)
	for i := range r.ChunkURLs {
		r.ChunkURLs[i] = "https://blob.example.invalid/x"
	}
	b, _ = json.Marshal(r)
	if _, err := Parse(Export, b); err == nil {
		t.Fatal("chunk cap removed")
	}
}

func TestContainerImplicitVolumeStillRequiresOrgConfinement(t *testing.T) {
	for _, kind := range []string{Start, Stop, Exec, Destroy} {
		f := newFake(t)
		f.volumes[testVolume] = otherOrg
		r := baseRequest()
		r.VolumeRef = ""
		if _, err := (&Engine{Runtime: f}).Run(context.Background(), kind, r); !errors.Is(err, ErrOwnership) || len(f.effects) != 0 {
			t.Fatal("implicit volume label was not checked", kind, err)
		}
	}
}

func TestContainerStandaloneOwnedVolumePurgeIsExplicit(t *testing.T) {
	f := newFake(t)
	r := baseRequest()
	r.ContainerRef = ""
	r.PurgeVolume = true
	b, _ := json.Marshal(r)
	parsed, err := Parse(Destroy, b)
	if err != nil {
		t.Fatal("standalone explicit purge refused", err)
	}
	if _, err = (&Engine{Runtime: f}).Run(context.Background(), Destroy, parsed); err != nil || strings.Join(f.effects, ",") != "destroy-volume" {
		t.Fatal("volume-only purge touched a container", err, f.effects)
	}
	r.PurgeVolume = false
	b, _ = json.Marshal(r)
	if _, err := Parse(Destroy, b); err == nil {
		t.Fatal("implicit volume purge allowed")
	}
}

func TestContainerReadRejectsGrowthAfterMetadataObservation(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "growing"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	observed, _ := f.Stat()
	if observed.Size() != 0 {
		t.Fatal("fixture")
	}
	if f.Truncate(FileLimit+1) != nil {
		t.Fatal("fixture")
	}
	if _, err := readVolumeBytes(f); !errors.Is(err, ErrBounds) {
		t.Fatal("file growth bypassed actual byte bound", err)
	}
}
func TestContainerImportRejectsNoncanonicalPathEvenWithinRoot(t *testing.T) {
	f := newFake(t)
	r := transferRequest()
	r.VolumeRef = newVolume
	r.ContainerRef = ""
	r.QuotaBytes = 16 << 20
	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	tw.WriteHeader(&tar.Header{Name: "a", Typeflag: tar.TypeDir, Mode: 0700})
	tw.WriteHeader(&tar.Header{Name: "a/../safe", Typeflag: tar.TypeReg, Mode: 0600, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	g, _ := transferCipher(r)
	nonce := bytes.Repeat([]byte{0x12}, 12)
	b := g.Seal(bytes.Clone(nonce), nonce, plain.Bytes(), chunkAAD(r.OrgID, r.TransferID, 0))
	digest := sha256.Sum256(b)
	r.Manifest = &Manifest{Version: 1, OrgID: r.OrgID, TransferID: r.TransferID, Chunks: []Chunk{{Index: 0, PlaintextBytes: int64(plain.Len()), SHA256: hex.EncodeToString(digest[:])}}}
	r.ManifestSHA256 = manifestDigest(r.Manifest)
	_, err := (&Engine{Runtime: f, Transport: &fakeTransport{objects: map[string][]byte{r.ChunkURLs[0]: b}}}).Run(context.Background(), Import, r)
	if err == nil {
		t.Fatal("noncanonical archive path was normalized and accepted")
	}
}

func TestContainerTransferPreservesSafeLinksAndRefusesOutsideLinks(t *testing.T) {
	f := newFake(t)
	dir := filepath.Join(f.root, testVolume)
	if os.WriteFile(filepath.Join(dir, "file"), []byte("persist"), 0600) != nil {
		t.Fatal("fixture")
	}
	if os.Link(filepath.Join(dir, "file"), filepath.Join(dir, "hard")) != nil || os.Symlink("file", filepath.Join(dir, "sym")) != nil {
		t.Skip("links unavailable")
	}
	r := transferRequest()
	tr := &fakeTransport{objects: map[string][]byte{}}
	e := &Engine{Runtime: f, Transport: tr}
	out, err := e.Run(context.Background(), Export, r)
	if err != nil {
		t.Fatal(err)
	}
	r.VolumeRef = newVolume
	r.ContainerRef = ""
	r.QuotaBytes = 16 << 20
	r.Manifest = out.Manifest
	r.ManifestSHA256 = out.ManifestSHA256
	if _, err = e.Run(context.Background(), Import, r); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.root, newVolume)
	linked, err := os.Readlink(filepath.Join(target, "sym"))
	if err != nil || linked != "file" {
		t.Fatal("safe symlink not preserved", err)
	}
	original, _ := os.Stat(filepath.Join(target, "file"))
	hard, _ := os.Stat(filepath.Join(target, "hard"))
	if !os.SameFile(original, hard) {
		t.Fatal("hardlink was duplicated")
	}
	if os.Symlink("../outside", filepath.Join(dir, "escape")) != nil {
		t.Fatal("fixture")
	}
	r = transferRequest()
	if _, err = e.Run(context.Background(), Export, r); err == nil {
		t.Fatal("outside link exported")
	}
}

func TestContainerCreateReusesOwnedImportedDiskWithoutFormatting(t *testing.T) {
	f := newFake(t)
	sentinel := filepath.Join(f.root, testVolume, "keep")
	if os.WriteFile(sentinel, []byte("imported bytes"), 0600) != nil {
		t.Fatal("fixture")
	}
	r := baseRequest()
	r.QuotaBytes = 16 << 20
	if _, err := (&Engine{Runtime: f}).Run(context.Background(), Create, r); err != nil || strings.Join(f.effects, ",") != "create-container" {
		t.Fatal("owned disk was recreated", err, f.effects)
	}
	b, _ := os.ReadFile(sentinel)
	if string(b) != "imported bytes" {
		t.Fatal("imported bytes changed")
	}
	f.volumes[testVolume] = otherOrg
	f.effects = nil
	if _, err := (&Engine{Runtime: f}).Run(context.Background(), Create, r); !errors.Is(err, ErrOwnership) || len(f.effects) != 0 {
		t.Fatal("foreign disk reused")
	}
}

func TestContainerNormalExecDoesNotInventPostExecutionRunningState(t *testing.T) {
	f := newFake(t)
	f.running = true
	r := baseRequest()
	r.TimeoutSeconds = 1
	r.Argv = []string{"fixture"}
	out, err := (&Engine{Runtime: f}).Run(context.Background(), Exec, r)
	if err != nil || out.Running != nil {
		t.Fatal("stale pre-exec state was published as post-exec state", err)
	}
}
