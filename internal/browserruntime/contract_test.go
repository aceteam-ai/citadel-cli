package browserruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func spec() RunSpec {
	return RunSpec{Image: Image + ":abc@sha256:" + strings.Repeat("0", 64), Network: NetworkName("s-1"), SessionID: "s-1", Purpose: "cobrowse", UID: 1000, GID: 1000, SeccompProfile: "/tmp/seccomp.json"}
}

func TestArgsContract(t *testing.T) {
	s := spec()
	s.ProfileDir = "/tmp/p"
	a, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	j := " " + strings.Join(a, " ") + " "
	for _, want := range []string{
		" --user 1000:1000 ", " --cap-drop ALL ", " no-new-privileges ", " --read-only ",
		" -p 127.0.0.1::8102 ", " -p 127.0.0.1::9223 ",
		" aceteam.browser-runtime.session=s-1 ", " aceteam.browser-runtime.purpose=cobrowse ",
		" --security-opt seccomp=/tmp/seccomp.json ", " aceteam.browser-runtime.browser=chromium ", " MEETING_BROWSER=chromium ", " MEETING_REQUIRE_SANDBOX=1 ",
		" --mount type=bind,src=/tmp/p,dst=/profile ", " --network aceteam-br-s-1 ", "/home/bot:rw,size=256m,mode=0700,uid=1000,gid=1000",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
	if a[len(a)-1] != s.Image {
		t.Errorf("image must be last, got %q", a[len(a)-1])
	}
	for _, bad := range []string{"userns", "--privileged", "--network host", ":/workspace "} {
		if strings.Contains(j, bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
}

func TestArgsNoProfileNoMount(t *testing.T) {
	a, _ := spec().Args()
	j := strings.Join(a, " ")
	if strings.Contains(j, " -v ") || strings.Contains(j, "type=bind") {
		t.Error("no bind mount expected without ProfileDir")
	}
	if !strings.Contains(j, "--tmpfs /profile:rw,size=512m,mode=0700,uid=1000,gid=1000") {
		t.Error("profile tmpfs expected without ProfileDir")
	}
}

func TestArgsRejects(t *testing.T) {
	cases := map[string]func(*RunSpec){
		"root":         func(s *RunSpec) { s.UID = 0 },
		"session":      func(s *RunSpec) { s.SessionID = "a b" },
		"purpose":      func(s *RunSpec) { s.Purpose = "x" },
		"image":        func(s *RunSpec) { s.Image = "" },
		"relmnt":       func(s *RunSpec) { s.ProfileDir = "rel" },
		"gid0":         func(s *RunSpec) { s.GID = 0 },
		"rootdir":      func(s *RunSpec) { s.ProfileDir = "/" },
		"unclean":      func(s *RunSpec) { s.ProfileDir = "/a/../b" },
		"comma":        func(s *RunSpec) { s.ProfileDir = "/a,b" },
		"newline":      func(s *RunSpec) { s.ProfileDir = "/a\nb" },
		"wsrel":        func(s *RunSpec) { s.WorkspaceMeetingsDir = "rel" },
		"wscomma":      func(s *RunSpec) { s.WorkspaceMeetingsDir = "/a,b" },
		"wsroot":       func(s *RunSpec) { s.WorkspaceMeetingsDir = "/" },
		"netempty":     func(s *RunSpec) { s.Network = "" },
		"nethost":      func(s *RunSpec) { s.Network = "host" },
		"netbridge":    func(s *RunSpec) { s.Network = "bridge" },
		"netdefault":   func(s *RunSpec) { s.Network = "default" },
		"netpodman":    func(s *RunSpec) { s.Network = "podman" },
		"netprivate":   func(s *RunSpec) { s.Network = "private" },
		"netmismatch":  func(s *RunSpec) { s.Network = NetworkName("other") },
		"imgevil":      func(s *RunSpec) { s.Image = Image + "-evil@sha256:" + strings.Repeat("0", 64) },
		"imgsub":       func(s *RunSpec) { s.Image = Image + "/sub@sha256:" + strings.Repeat("0", 64) },
		"imglegacyev":  func(s *RunSpec) { s.Image = LegacyImage + "-dev@sha256:" + strings.Repeat("0", 64) },
		"imgtrailing":  func(s *RunSpec) { s.Image = Image + "@sha256:" + strings.Repeat("0", 64) + "x" },
		"quote":        func(s *RunSpec) { s.ProfileDir = "/a\"b" },
		"netnone":      func(s *RunSpec) { s.Network = "none" },
		"netcontainer": func(s *RunSpec) { s.Network = "container:x" },
		"imgdash":      func(s *RunSpec) { s.Image = "-v" },
		"imgnodigest":  func(s *RunSpec) { s.Image = Image + ":abc" },
		"imgforeign":   func(s *RunSpec) { s.Image = "evil.io/x@sha256:" + strings.Repeat("0", 64) },
	}
	for n, f := range cases {
		s := spec()
		f(&s)
		if _, err := s.Args(); err == nil {
			t.Errorf("%s: expected error", n)
		}
	}
}

func TestParsePublishedPort(t *testing.T) {
	if p, err := ParsePublishedPort("127.0.0.1:49153\n"); err != nil || p != 49153 {
		t.Errorf("got %d %v", p, err)
	}
	for _, bad := range []string{"", "0.0.0.0:49153", ":::49153", "127.0.0.1:99999x", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:1\n0.0.0.0:2"} {
		if _, err := ParsePublishedPort(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestParsePublishedPortIPv6(t *testing.T) {
	if _, err := ParsePublishedPort("[::1]:40000"); err == nil {
		t.Error("IPv6-only must fail: contract publishes 127.0.0.1")
	}
	if p, err := ParsePublishedPort("[::1]:40001\n127.0.0.1:40000"); err != nil || p != 40000 {
		t.Errorf("dual binding: got %d %v", p, err)
	}
}

func TestNetworkName(t *testing.T) {
	if NetworkName("s-1") != "aceteam-br-s-1" {
		t.Error(NetworkName("s-1"))
	}
}

func TestSessionFilterArgs(t *testing.T) {
	a, err := SessionFilterArgs("s-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--filter", "label=aceteam.browser-runtime=1", "--filter", "label=aceteam.browser-runtime.session=s-1"}
	if strings.Join(a, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", a)
	}
	if _, err := SessionFilterArgs("a b"); err == nil {
		t.Error("invalid id must fail")
	}
}

func TestParseChromiumMajor(t *testing.T) {
	if n, err := ParseChromiumMajor("131\n"); err != nil || n != 131 {
		t.Error(n, err)
	}
	if _, err := ParseChromiumMajor(""); err == nil {
		t.Error("empty must fail")
	}
}

// The non-root supervisord config must equal the root one minus user= lines,
// comments and the pidfile move; otherwise the two drift.
func TestSupervisordVariantsInSync(t *testing.T) {
	root, err := os.ReadFile("../../services/meeting-service/supervisord.conf")
	if err != nil {
		t.Fatal(err)
	}
	nr, err := os.ReadFile("../../services/meeting-service/supervisord-nonroot.conf")
	if err != nil {
		t.Fatal(err)
	}
	strip := func(b string) string {
		var out []string
		for _, l := range strings.Split(b, "\n") {
			if strings.HasPrefix(l, "#") || strings.HasPrefix(l, "user=") || strings.HasPrefix(l, "pidfile=") {
				continue
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	if strip(string(root)) != strip(string(nr)) {
		t.Error("supervisord-nonroot.conf drifted from supervisord.conf")
	}
	if regexp.MustCompile(`(?m)^user=`).Match(nr) {
		t.Error("non-root config must not set user=")
	}
}

func TestDockerfileRecordsChromiumMajorLabel(t *testing.T) {
	b, err := os.ReadFile("../../services/meeting-service/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{ImageLabelChromiumMajor, ImageLabelContract, ChromiumMajorFileInImage, "supervisord-nonroot.conf"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("Dockerfile missing %s", w)
		}
	}
}

func TestArgsRunnerFields(t *testing.T) {
	s := spec()
	s.ProfileDir, s.StageDir = "/tmp/p", "/tmp/stage"
	s.Name, s.Owner, s.DiskCacheMB, s.UserNSKeepID = "aceteam-brc-s-1", "abc123", 256, true
	a, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	j := " " + strings.Join(a, " ") + " "
	for _, want := range []string{
		" --name aceteam-brc-s-1 ", " aceteam.browser-runtime.owner=abc123 ", " --pull=never ", " --userns=keep-id ",
		" MEETING_DISK_CACHE_MB=256 ", " --mount type=bind,src=/tmp/stage,dst=/citadel-stage,readonly ",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
	if a[len(a)-1] != s.Image {
		t.Error("image must be last")
	}
	// --pull=never is unconditional: the image is pinned by digest.
	b, _ := spec().Args()
	if !strings.Contains(strings.Join(b, " "), "--pull=never") {
		t.Error("--pull=never must always be set")
	}
	for name, mod := range map[string]func(*RunSpec){
		"name":  func(s *RunSpec) { s.Name = "a b" },
		"owner": func(s *RunSpec) { s.Owner = "a,b" },
		"stage": func(s *RunSpec) { s.StageDir = "rel" },
	} {
		bad := spec()
		mod(&bad)
		if _, err := bad.Args(); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestBrowserChoice(t *testing.T) {
	for _, b := range []string{"", "chromium"} {
		s := spec()
		s.Browser = b
		if _, err := s.Args(); err != nil {
			t.Errorf("browser %q: %v", b, err)
		}
	}
	for _, b := range []string{"cloakbrowser", "Chromium", "firefox", "chromium "} {
		s := spec()
		s.Browser = b
		if _, err := s.Args(); err == nil || !strings.Contains(err.Error(), "unsupported browser") {
			t.Errorf("browser %q must be refused with a clear error, got %v", b, err)
		}
	}
}

func TestSeccompPathRequired(t *testing.T) {
	for _, p := range []string{"", "rel.json", "/tmp/a,b", "/tmp/../tmp/x", "/tmp/a=b"} {
		s := spec()
		s.SeccompProfile = p
		if _, err := s.Args(); err == nil {
			t.Errorf("seccomp path %q must be rejected", p)
		}
	}
}

func TestSeccompProfilePinned(t *testing.T) {
	sum := sha256.Sum256(SeccompProfileJSON)
	if got := hex.EncodeToString(sum[:]); got != SeccompProfileSHA256 {
		t.Fatalf("seccomp.json changed: sha256 %s, pinned %s (update the pin only with review)", got, SeccompProfileSHA256)
	}
	var prof struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names    []string        `json:"names"`
			Action   string          `json:"action"`
			Args     json.RawMessage `json:"args"`
			Includes json.RawMessage `json:"includes"`
			Excludes json.RawMessage `json:"excludes"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(SeccompProfileJSON, &prof); err != nil {
		t.Fatal(err)
	}
	// Still a deny-by-default profile, not an allow-all one.
	if prof.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction %q", prof.DefaultAction)
	}
	// The namespace-sandbox syscalls are allowed unconditionally (no caps, args
	// or arch filters), and nothing that Docker's default withholds from an
	// unprivileged container is opened beyond those three.
	need := map[string]bool{"clone": false, "unshare": false, "chroot": false}
	for _, r := range prof.Syscalls {
		if r.Action != "SCMP_ACT_ALLOW" || len(r.Args) > 0 || len(r.Includes) > 0 || len(r.Excludes) > 0 {
			continue
		}
		for _, n := range r.Names {
			if _, ok := need[n]; ok {
				need[n] = true
			}
			for _, banned := range []string{"mount", "setns", "bpf", "ptrace", "perf_event_open", "pivot_root", "reboot", "kexec_load", "init_module"} {
				if n == banned {
					t.Errorf("syscall %q must not be unconditionally allowed", n)
				}
			}
		}
	}
	for n, ok := range need {
		if !ok {
			t.Errorf("%s must be unconditionally allowed for Chromium's sandbox", n)
		}
	}
}

func TestWriteSeccompProfile(t *testing.T) {
	dir := t.TempDir()
	p, err := WriteSeccompProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, SeccompFileName) {
		t.Errorf("path %s", p)
	}
	b, _ := os.ReadFile(p)
	if string(b) != string(SeccompProfileJSON) {
		t.Error("written profile differs from the pinned one")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if _, err := WriteSeccompProfile(dir); err == nil {
		t.Error("a pre-existing file must not be overwritten")
	}
}

func TestSandboxStaysOffInDockerfileUntilVerified(t *testing.T) {
	b, err := os.ReadFile("../../services/meeting-service/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	df := string(b)
	// The sandbox label is derived from a build arg that defaults to empty, so an
	// image is never marked sandboxed unless the build proved it.
	if !strings.Contains(df, `ARG SANDBOX_VERIFIED=""`) || !strings.Contains(df, `aceteam.browser-runtime.sandbox="${SANDBOX_VERIFIED}"`) {
		t.Error("Dockerfile must set the sandbox label from the SANDBOX_VERIFIED build arg (default empty)")
	}
	if strings.Contains(df, `sandbox="1"`) {
		t.Error("the sandbox label must never be hard-coded")
	}
	m, err := os.ReadFile("../../services/meeting-service/meetingd.py")
	if err != nil {
		t.Fatal(err)
	}
	// --no-sandbox is only ever appended behind the REQUIRE_SANDBOX switch.
	if strings.Count(string(m), `"--no-sandbox"`) != 1 || !strings.Contains(string(m), "if not REQUIRE_SANDBOX:\n        # Historical behaviour") {
		t.Error("build_chrome_args must add --no-sandbox only when MEETING_REQUIRE_SANDBOX is unset")
	}
}
