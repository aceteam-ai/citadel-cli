package browserruntime

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func spec() RunSpec {
	return RunSpec{Image: Image + ":abc@sha256:0", SessionID: "s-1", Purpose: "cobrowse", UID: 1000, GID: 1000}
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
		" -v /tmp/p:/profile ", "/home/bot:rw,mode=0700,uid=1000,gid=1000",
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
	if strings.Contains(strings.Join(a, " "), " -v ") {
		t.Error("no mount expected without ProfileDir")
	}
}

func TestArgsRejects(t *testing.T) {
	cases := map[string]func(*RunSpec){
		"root":    func(s *RunSpec) { s.UID = 0 },
		"session": func(s *RunSpec) { s.SessionID = "a b" },
		"purpose": func(s *RunSpec) { s.Purpose = "x" },
		"image":   func(s *RunSpec) { s.Image = "" },
		"mount":   func(s *RunSpec) { s.ProfileDir = "/a:/etc" },
		"relmnt":  func(s *RunSpec) { s.ProfileDir = "rel" },
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
	for _, bad := range []string{"", "0.0.0.0:49153", ":::49153", "127.0.0.1:99999x", "127.0.0.1:1\n0.0.0.0:2"} {
		if _, err := ParsePublishedPort(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
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
