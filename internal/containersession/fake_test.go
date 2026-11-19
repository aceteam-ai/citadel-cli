package containersession

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

const fixtureConfig = `sessions:
  - name: lane1
    harness: claude-code
    command: [claude, --model, opus]
    cwd: /home/agent/work/repo
    runtime:
      kind: container
      image: ghcr.io/aceteam-ai/session:1
      size: small
      on_boot: resume
    credentials:
      - name: claude-oauth
        path: /home/agent/.claude/.credentials.json
      - name: gh-token
        env: GH_TOKEN
`

func fixtureDecl(t *testing.T) declaredsession.Declaration {
	t.Helper()
	snap, err := declaredsession.DecodeConfig([]byte(fixtureConfig))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := snap.Lookup("lane1")
	if !ok {
		t.Fatal("missing fixture")
	}
	return d
}

type fakeContainer struct {
	id     string
	state  string
	code   int
	pid    int
	image  string
	labels map[string]string
	mounts []fakeMount
}

type fakeMount struct {
	kind, source, target, subpath string
}

// fakeEngine is an in-memory container engine behind Runner.Exec. Unlike a
// stub-writing fake, it models mounts: harnessWrite resolves a path INSIDE the
// container through the recorded --mount list to the host path that really
// backs it, so a test can prove where a harness's bytes end up. Like a real
// engine it refuses a bind whose host source does not exist.
type fakeEngine struct {
	mu          sync.Mutex
	containers  map[string]*fakeContainer
	volumes     map[string]map[string]string
	volumeDir   string
	calls       [][]string
	failRun     bool
	stopCode    int
	failInspect bool
	nextID      int
}

func newFakeEngine(t *testing.T) *fakeEngine {
	return &fakeEngine{containers: map[string]*fakeContainer{}, volumes: map[string]map[string]string{}, volumeDir: t.TempDir()}
}

func (f *fakeEngine) find(ref string) (string, *fakeContainer) {
	for n, c := range f.containers {
		if n == ref || c.id == ref {
			return n, c
		}
	}
	return "", nil
}

func parseMounts(args []string) []fakeMount {
	var out []fakeMount
	for i, a := range args {
		if a != "--mount" {
			continue
		}
		m := fakeMount{}
		for _, kv := range strings.Split(args[i+1], ",") {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "type":
				m.kind = v
			case "source":
				m.source = v
			case "target":
				m.target = v
			case "volume-subpath", "subpath":
				m.subpath = v
			}
		}
		out = append(out, m)
	}
	return out
}

func (f *fakeEngine) hostPath(mounts []fakeMount, containerPath string) (string, string, bool) {
	best := -1
	for i, m := range mounts {
		if (containerPath == m.target || strings.HasPrefix(containerPath, m.target+"/")) && (best < 0 || len(m.target) > len(mounts[best].target)) {
			best = i
		}
	}
	if best < 0 {
		return "", "", false
	}
	m := mounts[best]
	rest := strings.TrimPrefix(containerPath, m.target)
	switch m.kind {
	case "bind":
		return m.source + rest, m.target, true
	case "volume":
		return filepath.Join(f.volumeDir, m.subpath) + rest, m.target, true
	}
	return "", "", false
}

func (f *fakeEngine) exec(cmd *exec.Cmd) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	args := cmd.Args[1:]
	f.calls = append(f.calls, append([]string(nil), args...))
	notFound := func(what string) ([]byte, error) {
		return []byte("Error: No such " + what), errors.New("exit status 1")
	}
	switch {
	case args[0] == "inspect":
		if f.failInspect {
			return []byte("Cannot connect to the daemon"), errors.New("exit status 1")
		}
		_, c := f.find(args[len(args)-1])
		if c == nil {
			return notFound("object")
		}
		labels, _ := json.Marshal(c.labels)
		return []byte(fmt.Sprintf("%s|%d|%d|%s|%s\n", c.state, c.code, c.pid, c.image, labels)), nil
	case args[0] == "volume" && args[1] == "inspect":
		if _, ok := f.volumes[args[2]]; !ok {
			return notFound("volume")
		}
		return nil, nil
	case args[0] == "volume" && args[1] == "create":
		f.volumes[args[len(args)-1]] = labelsOf(args)
		return nil, nil
	case args[0] == "run" && indexOf(args, "--rm") != -1:
		// The init container: mkdir -p on the volume, as the session user.
		mk := indexOf(args, "-p")
		for _, dir := range args[mk+1:] {
			_ = os.MkdirAll(filepath.Join(f.volumeDir, strings.TrimPrefix(dir, "/home/agent")), 0o755)
		}
		return nil, nil
	case args[0] == "run":
		if f.failRun {
			return []byte("boom"), errors.New("exit status 125")
		}
		mounts := parseMounts(args)
		for _, m := range mounts {
			if m.kind == "bind" {
				if _, err := os.Stat(m.source); err != nil {
					return []byte("invalid mount config for type \"bind\": bind source path does not exist: " + m.source), errors.New("exit status 125")
				}
			}
			if m.kind == "volume" && m.subpath != "" {
				if _, err := os.Stat(filepath.Join(f.volumeDir, m.subpath)); err != nil {
					return []byte("volume subpath does not exist: " + m.subpath), errors.New("exit status 125")
				}
			}
		}
		name := valueOf(args, "--name")
		f.nextID++
		c := &fakeContainer{id: fmt.Sprintf("cid%04d", f.nextID), state: "running", pid: 4000 + f.nextID, labels: labelsOf(args), mounts: mounts}
		for i, a := range args {
			if a == "--entrypoint" {
				c.image = args[i+2]
			}
		}
		f.containers[name] = c
		return []byte("warning: noise\n" + c.id + "\n"), nil
	case args[0] == "stop":
		_, c := f.find(args[len(args)-1])
		if c == nil {
			return notFound("container")
		}
		c.state, c.code = "exited", f.stopCode
		return nil, nil
	case args[0] == "rm":
		n, c := f.find(args[len(args)-1])
		if c == nil {
			return notFound("container")
		}
		delete(f.containers, n)
		return nil, nil
	}
	return nil, fmt.Errorf("fakeEngine: unhandled %v", args)
}

func (f *fakeEngine) runCall(t *testing.T) []string {
	t.Helper()
	for _, c := range f.calls {
		if c[0] == "run" && indexOf(c, "--rm") == -1 {
			return c
		}
	}
	t.Fatal("no run call recorded")
	return nil
}

// harnessWrite is a fake in-container harness writing a file at containerPath.
// A rename-based writer stages a temp file beside the target and renames it
// over; across mounts that is EXDEV/EBUSY, as in a real kernel.
func (f *fakeEngine) harnessWrite(t *testing.T, name, containerPath string, data []byte, renameBased bool) error {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers["acesess-"+name]
	if c == nil {
		t.Fatal("no container")
	}
	target, tmount, ok := f.hostPath(c.mounts, containerPath)
	if !ok {
		return errors.New("path not mounted")
	}
	if !renameBased {
		return os.WriteFile(target, data, 0o600)
	}
	tmpC := containerPath + ".tmp123"
	tmp, smount, ok := f.hostPath(c.mounts, tmpC)
	if !ok || smount != tmount {
		return errors.New("EXDEV: temp file is on a different mount")
	}
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func indexOf(args []string, v string) int {
	for i, a := range args {
		if a == v {
			return i
		}
	}
	return -1
}

func valueOf(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func labelsOf(args []string) map[string]string {
	m := map[string]string{}
	for i, a := range args {
		if a == "--label" {
			k, v, _ := strings.Cut(args[i+1], "=")
			m[k] = v
		}
	}
	return m
}

type fixture struct {
	r       *Runner
	eng     *fakeEngine
	store   *SealedFileStore
	nodeDir string
	runBase string
	decl    declaredsession.Declaration
}

func newFixture(t *testing.T, podman bool) *fixture {
	t.Helper()
	nodeDir, runBase := t.TempDir(), t.TempDir()
	comp, err := nodevault.OpenKeyCompartment(filepath.Join(nodeDir, "session-vault.key"), true)
	if err != nil {
		t.Fatal(err)
	}
	store := &SealedFileStore{Dir: filepath.Join(nodeDir, "session-credentials"), Sealer: comp}
	eng := newFakeEngine(t)
	rt := catalog.ContainerRuntime{EngineBin: "docker", Bin: "docker"}
	if podman {
		rt = catalog.ContainerRuntime{EngineBin: "podman", Bin: "podman"}
	}
	binary := filepath.Join(t.TempDir(), "citadel")
	if err := os.WriteFile(binary, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		RT: rt, Exec: eng.exec, Creds: store, NodeDir: nodeDir, RunBase: runBase,
		IsTmpfs: func(string) (bool, error) { return true, nil },
		Binary:  binary, UID: os.Getuid() + 1000, GID: os.Getgid() + 1000,
		Rand: strings.NewReader(strings.Repeat("\x07", 256)),
	}
	return &fixture{r: r, eng: eng, store: store, nodeDir: nodeDir, runBase: runBase, decl: fixtureDecl(t)}
}

const (
	oauthSecret = "OAUTH-SECRET-VALUE-0123456789"
	ghSecret    = "ghp_SECRETTOKENVALUE"
)

func (f *fixture) seed(t *testing.T) {
	t.Helper()
	if err := f.store.Save("lane1", Bundle{"claude-oauth": []byte(oauthSecret), "gh-token": []byte(ghSecret)}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) runDir(t *testing.T) string {
	t.Helper()
	d, err := f.r.RunDir("lane1")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func walkContains(t *testing.T, root string, needles ...string) []string {
	t.Helper()
	var hits []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		raw, _ := os.ReadFile(p)
		for _, n := range needles {
			if strings.Contains(string(raw), n) {
				hits = append(hits, p+" contains "+n[:6])
			}
		}
		return nil
	})
	sort.Strings(hits)
	return hits
}
