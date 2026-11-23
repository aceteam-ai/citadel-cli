package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This implements the actual ContainerEngine boundary, but executes only this
// test binary. No daemon, registry, image pull, or browser is involved.
type platformFixtureEngine struct {
	info, image, manifest, calls string
	missing, podman              bool
}

func (e platformFixtureEngine) IsPodman() bool { return e.podman }
func (e platformFixtureEngine) Label() string  { return "fixture" }
func (e platformFixtureEngine) EngineCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestContainerPlatformEngineProcess$", "--"}, args...)...)
	data, _ := json.Marshal([]any{e.info, e.image, e.manifest, e.calls, e.missing})
	cmd.Env = append(os.Environ(), "CITADEL_PLATFORM_ENGINE_FIXTURE="+string(data), "GORACE=atexit_sleep_ms=0")
	return cmd
}

func TestContainerPlatformEngineProcess(t *testing.T) {
	data := os.Getenv("CITADEL_PLATFORM_ENGINE_FIXTURE")
	if data == "" {
		return
	}
	var fixture []json.RawMessage
	if err := json.Unmarshal([]byte(data), &fixture); err != nil {
		os.Exit(2)
	}
	var info, image, manifest, calls string
	var missing bool
	_ = json.Unmarshal(fixture[0], &info)
	_ = json.Unmarshal(fixture[1], &image)
	_ = json.Unmarshal(fixture[2], &manifest)
	_ = json.Unmarshal(fixture[3], &calls)
	_ = json.Unmarshal(fixture[4], &missing)
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	previous, _ := os.ReadFile(calls)
	f, err := os.OpenFile(calls, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	record, _ := json.Marshal(args)
	_, _ = fmt.Fprintln(f, string(record))
	_ = f.Close()
	switch {
	case args[0] == "info":
		fmt.Print(info)
	case args[0] == "manifest":
		fmt.Print(manifest)
	case args[0] == "pull":
	case args[0] == "image":
		if missing && !bytes.Contains(previous, []byte(`["pull",`)) {
			os.Exit(1)
		}
		if args[3] == imageLabelFormat {
			fmt.Print("1|1")
		} else {
			fmt.Print(image)
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestContainerPlatformEngineCompatibility(t *testing.T) {
	const dockerInfo = `{"OSType":"linux","Architecture":"x86_64"}`
	const amd64Image = `{"Os":"linux","Architecture":"amd64"}`
	const arm64Image = `{"Os":"linux","Architecture":"arm64","Variant":"v8"}`
	const single = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:fixture"}}`
	child := "sha256:" + strings.Repeat("a", 64)
	index := func(arch string) string {
		return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":%q,"platform":{"os":"linux","architecture":%q}}]}`, child, arch)
	}
	for _, tc := range []struct {
		name, info, image, manifest string
		missing, podman, pull       bool
		wantErr, wantRef            string
	}{
		{name: "local single match", info: dockerInfo, image: amd64Image},
		{name: "local single arch mismatch", info: dockerInfo, image: arm64Image, wantErr: "does not match"},
		{name: "local single OS mismatch", info: dockerInfo, image: `{"os":"windows","architecture":"amd64"}`, wantErr: "does not match"},
		{name: "variant mismatch", info: dockerInfo, image: `{"os":"linux","architecture":"amd64","variant":"v3"}`, wantErr: "does not match"},
		{name: "unknown image platform", info: dockerInfo, image: `{}`, wantErr: "unknown"},
		{name: "unknown engine platform", info: `{}`, image: amd64Image, wantErr: "unknown"},
		{name: "podman arm64 baseline", podman: true, info: `{"host":{"os":"linux","arch":"aarch64"}}`, image: arm64Image},
		{name: "absent discovery", info: dockerInfo, image: amd64Image, missing: true, manifest: index("amd64"), wantErr: "not yet known"},
		{name: "matching index child", info: dockerInfo, image: amd64Image, missing: true, pull: true, manifest: index("amd64"), wantRef: strings.Split(testImage, "@")[0] + "@" + child},
		{name: "index lacks matching child", info: dockerInfo, image: amd64Image, missing: true, pull: true, manifest: index("arm64"), wantErr: "no entry"},
		{name: "single pull match", info: dockerInfo, image: amd64Image, missing: true, pull: true, manifest: single, wantRef: testImage},
		{name: "single pull mismatch", info: dockerInfo, image: arm64Image, missing: true, pull: true, manifest: single, wantErr: "does not match"},
		{name: "resolved child still checked", info: dockerInfo, image: arm64Image, missing: true, pull: true, manifest: index("amd64"), wantErr: "does not match"},
		{name: "malformed manifest", info: dockerInfo, image: amd64Image, missing: true, pull: true, manifest: `{`, wantErr: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			e := platformFixtureEngine{tc.info, tc.image, tc.manifest, calls, tc.missing, tc.podman}
			r := newContainerRunner(BrowserRuntimeConfig{Engine: e, Image: testImage})
			r.uid, r.gid = 1000, 1000
			err := r.healthy(tc.pull)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("got %v, want %q", err, tc.wantErr)
			}
			if tc.wantRef != "" && r.imageForRun() != tc.wantRef {
				t.Fatalf("selected %s, want %s", r.imageForRun(), tc.wantRef)
			}
			record, _ := os.ReadFile(calls)
			if !tc.pull && (bytes.Contains(record, []byte(`["pull",`)) || bytes.Contains(record, []byte(`["manifest",`))) {
				t.Fatalf("discovery must stay local: %s", record)
			}
			if tc.wantRef != "" && !bytes.Contains(record, []byte(`["pull","--platform","linux/amd64","`+tc.wantRef+`"]`)) {
				t.Fatalf("pull must use selected platform and digest: %s", record)
			}
			if tc.wantErr == "no entry" && bytes.Contains(record, []byte(`["pull",`)) {
				t.Fatalf("mismatched index must not pull: %s", record)
			}
		})
	}
}

func TestContainerPlatformFallbackLogsOnce(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			f := &fakeEngine{}
			r := newTestRunner(f, false)
			base := r.eng
			r.eng = func(ctx context.Context, args ...string) (string, error) {
				if args[0] == "image" && missing {
					return "", fmt.Errorf("not present")
				}
				if args[0] == "image" && args[3] == "{{json .}}" {
					return `{"os":"linux","architecture":"arm64"}`, nil
				}
				return base(ctx, args...)
			}
			var output bytes.Buffer
			writer := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(writer)
			n := 0
			s := &selectingRunner{host: markerRunner{"host", &n}, container: r}
			for i := 0; i < 3; i++ {
				if s.useContainer(true) {
					t.Fatal("must fall back")
				}
			}
			if s.Name() != "host" {
				t.Fatal("must discover host")
			}
			if strings.Count(output.String(), "using the host browser runner") != 1 {
				t.Fatalf("must log once: %s", &output)
			}
			if f.has("pull") != 0 || f.has("run") != 0 || f.has("network", "create") != 0 {
				t.Fatalf("fallback must not launch or pull: %v", f.calls)
			}
		})
	}
}

func TestContainerPlatformUnknownDiscoveryCanBeExplicitlyRetried(t *testing.T) {
	f := &fakeEngine{failOn: "image inspect"}
	r := newTestRunner(f, false)
	if err := r.healthy(false); err != errContainerImageUnknown {
		t.Fatal(err)
	}
	f.failOn = ""
	if err := r.healthy(true); err != nil {
		t.Fatal("unknown discovery must not poison explicit retry:", err)
	}
}

func TestContainerPlatformResolvedChildUsedForLaunch(t *testing.T) {
	f := &fakeEngine{}
	r := newTestRunner(f, false)
	base := r.eng
	pulled := false
	child := "sha256:" + strings.Repeat("a", 64)
	selected := strings.Split(testImage, "@")[0] + "@" + child
	r.eng = func(ctx context.Context, args ...string) (string, error) {
		switch args[0] {
		case "manifest":
			return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"arm64"}},{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}]}`, child, child), nil
		case "pull":
			if args[len(args)-1] != selected {
				t.Fatalf("wrong pull: %v", args)
			}
			pulled = true
		case "image":
			if !pulled {
				return "", fmt.Errorf("not present")
			}
			if args[len(args)-1] != selected {
				t.Fatalf("wrong inspection: %v", args)
			}
		}
		return base(ctx, args...)
	}
	if _, err := r.Launch(profileFor(t), "", LaunchOptions{UseContainer: true}); err != nil {
		t.Fatal(err)
	}
	for _, args := range f.calls {
		if args[0] == "run" {
			if args[len(args)-1] != selected {
				t.Fatalf("must run selected child, not index: %v", args)
			}
			return
		}
	}
	t.Fatal("missing fake run")
}

func TestContainerPlatformIndexRejectsInvalidDigest(t *testing.T) {
	target := containerImagePlatform{OS: "linux", Architecture: "amd64"}
	for _, digest := range []string{"sha256:bad", "sha256:" + strings.Repeat("z", 64), "sha512:" + strings.Repeat("a", 64)} {
		manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}]}`, digest)
		if _, err := resolveImagePlatform(testImage, manifest, target); err == nil {
			t.Fatalf("accepted %q", digest)
		}
	}
}
