package containersession

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	state  string
	code   int
	labels map[string]string
}

// fakeEngine is an in-memory container engine behind Runner.Exec. It records
// every argv and, like a real engine, leaves an EMPTY mountpoint stub on the
// volume for every bind mount below the volume's mount point.
type fakeEngine struct {
	mu          sync.Mutex
	containers  map[string]*fakeContainer
	volumes     map[string]map[string]string
	volumeDir   string
	calls       [][]string
	failRun     bool
	stopCode    int
	failInspect bool
}

func newFakeEngine(t *testing.T) *fakeEngine {
	return &fakeEngine{containers: map[string]*fakeContainer{}, volumes: map[string]map[string]string{}, volumeDir: t.TempDir()}
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
		c, ok := f.containers[args[len(args)-1]]
		if !ok {
			return notFound("object")
		}
		labels, _ := json.Marshal(c.labels)
		return []byte(fmt.Sprintf("%s|%d|%s\n", c.state, c.code, labels)), nil
	case args[0] == "volume" && args[1] == "inspect":
		if _, ok := f.volumes[args[2]]; !ok {
			return notFound("volume")
		}
		return nil, nil
	case args[0] == "volume" && args[1] == "create":
		f.volumes[args[len(args)-1]] = labelsOf(args)
		return nil, nil
	case args[0] == "run":
		if f.failRun {
			return []byte("boom"), errors.New("exit status 125")
		}
		name := valueOf(args, "--name")
		f.containers[name] = &fakeContainer{state: "running", labels: labelsOf(args)}
		for i, a := range args {
			if a == "-v" {
				parts := strings.Split(args[i+1], ":")
				if len(parts) >= 2 && strings.HasPrefix(parts[1], "/home/agent/") {
					stub := filepath.Join(f.volumeDir, strings.TrimPrefix(parts[1], "/home/agent/"))
					_ = os.MkdirAll(filepath.Dir(stub), 0o755)
					_ = os.WriteFile(stub, nil, 0o644) // mountpoint stub: empty
				}
			}
		}
		return []byte("id\n"), nil
	case args[0] == "stop":
		c, ok := f.containers[args[len(args)-1]]
		if !ok {
			return notFound("container")
		}
		c.state, c.code = "exited", f.stopCode
		return nil, nil
	case args[0] == "rm":
		name := args[len(args)-1]
		if _, ok := f.containers[name]; !ok {
			return notFound("container")
		}
		delete(f.containers, name)
		return nil, nil
	}
	return nil, fmt.Errorf("fakeEngine: unhandled %v", args)
}

func (f *fakeEngine) runCall(t *testing.T) []string {
	t.Helper()
	for _, c := range f.calls {
		if c[0] == "run" {
			return c
		}
	}
	t.Fatal("no run call recorded")
	return nil
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
	sess, err := nodevault.OpenKeyCompartment(filepath.Join(nodeDir, "session-vault.key"), true)
	if err != nil {
		t.Fatal(err)
	}
	store := &SealedFileStore{Dir: filepath.Join(nodeDir, "session-credentials"), Sealer: sess}
	eng := newFakeEngine(t)
	rt := catalog.ContainerRuntime{EngineBin: "docker", Bin: "docker"}
	if podman {
		rt = catalog.ContainerRuntime{EngineBin: "podman", Bin: "podman"}
	}
	r := &Runner{
		RT: rt, Exec: eng.exec, Creds: store, NodeDir: nodeDir, RunBase: runBase,
		IsTmpfs: func(string) (bool, error) { return true, nil },
		Binary:  "/opt/node/citadel", UID: 1000, GID: 1000,
		Rand: strings.NewReader(strings.Repeat("\x07", 64)),
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

var _ = strconv.Itoa
