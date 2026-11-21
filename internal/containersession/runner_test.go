package containersession

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

func hasPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

func TestWakeRunArgvDocker(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	rec, err := f.r.Wake(context.Background(), f.decl)
	if err != nil {
		t.Fatal(err)
	}
	args := f.eng.runCall(t)
	for _, pair := range [][2]string{
		{"--name", "acesess-lane1"}, {"--restart", "no"}, {"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"}, {"--cpus", "1"}, {"--memory", "1g"}, {"--pids-limit", "512"},
		{"--entrypoint", BinaryMount},
	} {
		if !hasPair(args, pair[0], pair[1]) {
			t.Errorf("run argv missing %v: %v", pair, args)
		}
	}
	if indexOf(args, "--userns") != -1 {
		t.Error("docker run must not carry --userns")
	}
	if indexOf(args, "-v") != -1 || valueOf(args, "--tmpfs") != "/tmp:rw,mode=1777" {
		t.Errorf("--mount only, and no tmpfs over /run/session: %v", args)
	}
	runDir := f.runDir(t)
	view := f.r.ViewDir("lane1")
	wantMounts := []string{
		"type=volume,source=acesess-lane1-home,target=/home/agent",
		"type=bind,source=" + filepath.Join(runDir, "cfg") + ",target=/home/agent/.claude",
		"type=volume,source=acesess-lane1-home,target=/home/agent/.claude/projects,volume-subpath=.claude/projects",
		"type=volume,source=acesess-lane1-home,target=/home/agent/.claude/file-history,volume-subpath=.claude/file-history",
		"type=bind,source=" + view + ",target=/run/session",
		"type=bind,source=" + f.r.Binary + ",target=" + BinaryMount + ",readonly",
		"type=bind,source=" + filepath.Join(runDir, "envdir") + ",target=" + declaredsession.ContainerCredDir + ",readonly",
	}
	for _, m := range wantMounts {
		if !hasPair(args, "--mount", m) {
			t.Errorf("missing mount %q in %v", m, args)
		}
	}
	// The image ends the engine flags; the holder argv follows it.
	img := indexOf(args, "ghcr.io/aceteam-ai/session:1")
	if img == -1 || strings.Join(args[img+1:], " ") != "session-host lane1 --container --node-config-dir /run/session --launch-id "+rec.Incarnation {
		t.Errorf("image/holder tail wrong: %v", args[img:])
	}
	for _, a := range args {
		if strings.Contains(a, oauthSecret) || strings.Contains(a, ghSecret) {
			t.Fatalf("secret in argv: %q", a)
		}
	}
	if v := f.eng.volumes["acesess-lane1-home"]; v[LabelSession] != "lane1" || v[LabelVolume] != "acesess-lane1-home" {
		t.Errorf("volume labels = %v", v)
	}
}

func TestPersistedSubpathsPerHarnessAndEngineKey(t *testing.T) {
	f := newFixture(t, true)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	args := f.eng.runCall(t)
	if !hasPair(args, "--mount", "type=volume,source=acesess-lane1-home,target=/home/agent/.claude/projects,subpath=.claude/projects") {
		t.Errorf("podman must use the subpath key: %v", args)
	}
	// Every pinned subpath is a volume mount on top of the tmpfs config dir.
	cfg, _ := declaredsession.HarnessConfigFor("claude-code")
	for _, sub := range cfg.Persist {
		want := "type=volume,source=acesess-lane1-home,target=/home/agent/.claude/" + sub + ",subpath=.claude/" + sub
		if !hasPair(args, "--mount", want) {
			t.Errorf("missing persisted subpath %s", sub)
		}
	}
	// The init container created them as the session user, with no capabilities.
	var init []string
	for _, c := range f.eng.calls {
		if c[0] == "run" && indexOf(c, "--rm") != -1 {
			init = c
		}
	}
	if init == nil || !hasPair(init, "--network", "none") || !hasPair(init, "--cap-drop", "ALL") || !hasPair(init, "--entrypoint", "/bin/mkdir") || indexOf(init, "/home/agent/.claude/projects") == -1 {
		t.Errorf("init container argv = %v", init)
	}
	codex, _ := declaredsession.HarnessConfigFor("codex")
	if codex.Dir != "/home/agent/.codex" || indexOf(codex.Persist, "sessions") == -1 {
		t.Errorf("codex layout = %+v", codex)
	}
}

func TestWakeRunArgvPodmanKeepsID(t *testing.T) {
	f := newFixture(t, true)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	if !hasPair(f.eng.runCall(t), "--userns", "keep-id") {
		t.Error("podman run must keep the host uid mapping")
	}
	for _, c := range f.eng.calls {
		if c[0] == "run" || c[0] == "inspect" {
			continue
		}
	}
}

func TestLabelRecoveryAndTamperRefusal(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	rec, err := f.r.Wake(context.Background(), f.decl)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := f.r.Observe(context.Background(), "lane1")
	if err != nil || !obs.Exists || obs.State != "running" || !obs.HasRecord {
		t.Fatalf("observe = %+v, %v", obs, err)
	}
	if obs.Record != rec || obs.Record.Revision != f.decl.Revision || obs.Record.Volume != "acesess-lane1-home" {
		t.Fatalf("recovered %+v want %+v", obs.Record, rec)
	}
	good := rec.Labels()
	for name, mutate := range map[string]func(map[string]string){
		"volume of another session": func(m map[string]string) { m[LabelVolume] = "acesess-other-home" },
		"unknown schema":            func(m map[string]string) { m[LabelSchema] = "9" },
		"no revision":               func(m map[string]string) { m[LabelRevision] = "" },
		"short incarnation":         func(m map[string]string) { m[LabelIncarnation] = "abc" },
	} {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		mutate(m)
		if _, ok := RecordFromLabels(m); ok {
			t.Errorf("%s: tampered labels accepted", name)
		}
	}
	// A same-named container with no record is never adopted or replaced.
	f.eng.containers["acesess-lane1"].labels = map[string]string{}
	f.eng.containers["acesess-lane1"].state = "exited"
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrSpec) {
		t.Fatalf("foreign container wake err = %v", err)
	}
	if err := f.r.Sleep(context.Background(), f.decl); !errors.Is(err, ErrSpec) {
		t.Fatalf("foreign container sleep err = %v", err)
	}
}

func TestCredentialMaterializeAndResealRoundTrip(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	runDir := f.runDir(t)
	got, err := os.ReadFile(filepath.Join(runDir, "cfg", ".credentials.json"))
	if err != nil || string(got) != oauthSecret {
		t.Fatalf("materialized file = %q, %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(runDir, "cfg", ".credentials.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode = %v", info.Mode().Perm())
	}
	env, _ := os.ReadFile(filepath.Join(runDir, "envdir", "credentials.env"))
	if string(env) != "GH_TOKEN="+ghSecret+"\n" {
		t.Errorf("env file = %q", env)
	}
	// The harness refreshes its OAuth token in place while running.
	rotated := "ROTATED-" + oauthSecret
	if err := f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/.credentials.json", []byte(rotated), true); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Sleep(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	b, err := f.store.Load("lane1")
	if err != nil || string(b["claude-oauth"]) != rotated || string(b["gh-token"]) != ghSecret {
		t.Fatalf("resealed bundle = %v, %v", b, err)
	}
	if HasMaterialized(runDir) {
		t.Error("run dir still holds secrets after sleep")
	}
	if _, ok := f.eng.containers["acesess-lane1"]; ok {
		t.Error("sleep must remove the container")
	}
	if _, ok := f.eng.volumes["acesess-lane1-home"]; !ok {
		t.Error("sleep must keep the volume")
	}
	// Next wake materializes the rotated value, not the original.
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(runDir, "cfg", ".credentials.json"))
	if string(got) != rotated {
		t.Errorf("second wake materialized %q", got)
	}
}

func TestSleepResealFailureKeepsSecretsForRetry(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	runDir := f.runDir(t)
	_ = f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/.credentials.json", []byte("NEW"), true)
	f.r.Creds = failingSave{f.store}
	if err := f.r.Sleep(context.Background(), f.decl); !errors.Is(err, ErrReseal) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(runDir, "cfg", ".credentials.json")); string(got) != "NEW" {
		t.Error("rotated token was dropped on a failed re-seal")
	}
	f.r.Creds = f.store
	if err := f.r.Sleep(context.Background(), f.decl); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if b, _ := f.store.Load("lane1"); string(b["claude-oauth"]) != "NEW" {
		t.Error("retry did not seal the rotated token")
	}
}

type failingSave struct{ CredentialStore }

func (failingSave) Save(string, Bundle) error { return errors.New("disk full") }

func TestNoCredentialBytesUnderVolumeEvenWithRenameBasedRotation(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	needles := []string{oauthSecret, ghSecret}
	// A fake harness inside the container rotates its token the way real
	// harnesses do: stage a temp file beside the credential file, rename over.
	rotated := "ROTATED-" + oauthSecret
	if err := f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/.credentials.json", []byte(rotated), true); err != nil {
		t.Fatalf("rename-based rotation failed: %v", err)
	}
	// Transcripts, by contrast, must land on the volume.
	if err := f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/projects/p/s.jsonl", []byte("transcript"), true); err != nil {
		t.Fatalf("transcript write failed: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(f.eng.volumeDir, ".claude", "projects", "p", "s.jsonl")); err != nil || string(raw) != "transcript" {
		t.Fatalf("transcript is not on the volume: %q %v", raw, err)
	}
	needles = append(needles, rotated)
	if hits := walkContains(t, f.eng.volumeDir, needles...); len(hits) != 0 {
		t.Fatalf("credential bytes on the volume: %v", hits)
	}
	// The harness's staging temp file never touched the volume either.
	_ = filepath.Walk(f.eng.volumeDir, func(p string, _ os.FileInfo, _ error) error {
		if strings.Contains(p, ".credentials") {
			t.Errorf("credential file path on the volume: %s", p)
		}
		return nil
	})
	if hits := walkContains(t, f.nodeDir, needles...); len(hits) != 0 {
		t.Fatalf("credential plaintext under the node dir: %v", hits)
	}
	cfg, _ := os.ReadFile(filepath.Join(f.r.ViewDir("lane1"), "sessions.yaml"))
	for _, n := range needles {
		if strings.Contains(string(cfg), n) {
			t.Fatal("secret in rendered config")
		}
	}
	// Plaintext exists only under the tmpfs run base, and sleep removes it after
	// re-sealing the rotation.
	if hits := walkContains(t, f.runBase, needles...); len(hits) == 0 {
		t.Fatal("expected the secrets on the tmpfs run dir")
	}
	if err := f.r.Sleep(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	if hits := walkContains(t, f.runBase, needles...); len(hits) != 0 {
		t.Fatalf("secrets remain after sleep: %v", hits)
	}
	if b, _ := f.store.Load("lane1"); string(b["claude-oauth"]) != rotated {
		t.Error("rename-based rotation was not re-sealed")
	}
}

func TestSingleFileMountWouldHaveLeakedToVolume(t *testing.T) {
	// Negative control for the test above: a credential mounted as one file on
	// top of the volume stages its rename temp file ON the volume. The fake's
	// mount resolver must see that, or the scan above proves nothing.
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	c := f.eng.containers["acesess-lane1"]
	c.mounts = []fakeMount{{kind: "volume", source: "v", target: "/home/agent"},
		{kind: "bind", source: filepath.Join(f.runDir(t), "cfg", ".credentials.json"), target: "/home/agent/.claude/.credentials.json"}}
	if err := f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/.credentials.json", []byte("LEAK-CHECK-VALUE"), true); err == nil {
		t.Fatal("a single-file mount must make the rename-based writer fail (EXDEV)")
	}
}

func TestMountTreeIsBuiltOnTheHostWithNodeUserOwnership(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	view := f.r.ViewDir("lane1")
	for _, dir := range []string{view, filepath.Join(view, "sessions"), f.runDir(t), filepath.Join(f.runDir(t), "cfg"), filepath.Join(f.runDir(t), "envdir")} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v err = %v, want 0700", dir, info, err)
		}
	}
	if info, err := os.Stat(filepath.Join(view, "sessions.yaml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("sessions.yaml = %v, %v", info, err)
	}
	// Exactly one bind at /run/session and nothing nested under it, so the
	// engine has no parent directory of its own to create.
	n := 0
	for _, m := range f.eng.containers["acesess-lane1"].mounts {
		if m.target == "/run/session" {
			n++
		}
		if strings.HasPrefix(m.target, "/run/session/") {
			t.Errorf("nested mount under /run/session: %s", m.target)
		}
	}
	if n != 1 {
		t.Errorf("/run/session mounts = %d", n)
	}
	// Every persisted-subpath mountpoint exists on the host tmpfs, created by us.
	for _, sub := range []string{"projects", "file-history"} {
		if info, err := os.Stat(filepath.Join(f.runDir(t), "cfg", sub)); err != nil || !info.IsDir() {
			t.Errorf("mountpoint %s missing: %v", sub, err)
		}
	}
	// And the volume side exists too (init container), so subpaths resolve.
	if _, err := os.Stat(filepath.Join(f.eng.volumeDir, ".claude", "projects")); err != nil {
		t.Errorf("volume projects dir missing: %v", err)
	}
}

func TestMissingBindSourceFailsInsteadOfBecomingEmptyDir(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	f.r.Binary = filepath.Join(t.TempDir(), "gone")
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrEngine) {
		t.Fatalf("err = %v", err)
	}
	if HasMaterialized(f.runDir(t)) {
		t.Error("secrets left after refused mount")
	}
}

func TestConcurrentWakeAndSleepAreRefusedAndHarmless(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	runDir := f.runDir(t)
	if err := f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/.credentials.json", []byte("ROTATED-LIVE"), true); err != nil {
		t.Fatal(err)
	}
	// Another actor (the reconciler, a manual wake) holds the session.
	release, err := acquireLock(filepath.Join(runDir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	n := len(f.eng.calls)
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrBusy) {
		t.Errorf("wake err = %v", err)
	}
	if err := f.r.Sleep(context.Background(), f.decl); !errors.Is(err, ErrBusy) {
		t.Errorf("sleep err = %v", err)
	}
	if len(f.eng.calls) != n {
		t.Errorf("a refused wake/sleep touched the engine: %v", f.eng.calls[n:])
	}
	if got, _ := os.ReadFile(filepath.Join(runDir, "cfg", ".credentials.json")); string(got) != "ROTATED-LIVE" {
		t.Errorf("live credential file was disturbed: %q", got)
	}
	if _, ok := f.eng.containers["acesess-lane1"]; !ok {
		t.Error("live container was removed")
	}
	// A re-seal tick during the contention just skips.
	if err := f.r.ResealOnce(f.decl); err != nil {
		t.Errorf("reseal under contention = %v", err)
	}
	release()
	if err := f.r.Sleep(context.Background(), f.decl); err != nil {
		t.Fatalf("sleep after release: %v", err)
	}
	if b, _ := f.store.Load("lane1"); string(b["claude-oauth"]) != "ROTATED-LIVE" {
		t.Error("rotation lost")
	}
}

func TestFailedWakeRemovesOnlyItsOwnContainerById(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	f.r.WaitReady = func(context.Context, string, string) error { return errors.New("no hello") }
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrEngine) {
		t.Fatalf("err = %v", err)
	}
	var rms [][]string
	for _, c := range f.eng.calls {
		if c[0] == "rm" || c[0] == "stop" {
			rms = append(rms, c)
		}
	}
	if len(rms) == 0 {
		t.Fatal("no cleanup")
	}
	for _, c := range rms {
		if got := c[len(c)-1]; !strings.HasPrefix(got, "cid") {
			t.Errorf("cleanup addressed %q, want the container id this wake started", got)
		}
	}
	// A run that failed outright has no id of its own to remove.
	f2 := newFixture(t, false)
	f2.seed(t)
	f2.eng.failRun = true
	_, _ = f2.r.Wake(context.Background(), f2.decl)
	for _, c := range f2.eng.calls {
		if c[0] == "rm" && c[len(c)-1] == "acesess-lane1" && len(f2.eng.containers) != 0 {
			t.Error("removed by name without an id of our own")
		}
	}
}

func TestRotationSurvivesRebootViaPeriodicReseal(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	if err := f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/.credentials.json", []byte("ROTATED-AWAKE"), true); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ResealOnce(f.decl); err != nil { // the worker's poll tick
		t.Fatal(err)
	}
	if b, _ := f.store.Load("lane1"); string(b["claude-oauth"]) != "ROTATED-AWAKE" {
		t.Fatal("awake re-seal did not capture the rotation")
	}
	// Reboot: tmpfs gone, container killed. No sleep ever ran.
	runDir := f.runDir(t)
	if err := os.RemoveAll(filepath.Join(runDir, "cfg")); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(runDir, "envdir"))
	c := f.eng.containers["acesess-lane1"]
	c.state, c.code = "exited", 137
	out := f.r.Reconcile(context.Background(), declaredsession.Snapshot{Sessions: []declaredsession.Declaration{f.decl}})
	if len(out) != 1 || !out[0].Decision.Wake || out[0].Err != nil {
		t.Fatalf("reconcile = %+v", out)
	}
	got, _ := os.ReadFile(filepath.Join(runDir, "cfg", ".credentials.json"))
	if string(got) != "ROTATED-AWAKE" {
		t.Fatalf("after reboot the harness got %q, not the rotated token", got)
	}
}

func TestPausedContainerIsNeverReplaced(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	f.eng.containers["acesess-lane1"].state = "paused"
	n := len(f.eng.calls)
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrSpec) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.eng.calls[n:] {
		if c[0] == "rm" || c[0] == "run" || c[0] == "stop" {
			t.Fatalf("paused container disturbed: %v", c)
		}
	}
}

func TestFailedSleepIsNotUndoneByTheBootReconciler(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	f.eng.stopCode = 137
	f.r.Creds = failingSave{f.store}
	_ = f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/.credentials.json", []byte("NEW"), true)
	if err := f.r.Sleep(context.Background(), f.decl); !errors.Is(err, ErrReseal) {
		t.Fatalf("err = %v", err)
	}
	if c := f.eng.containers["acesess-lane1"]; c == nil || c.state != "exited" || c.code == 0 {
		t.Fatalf("expected an exited non-zero container, got %+v", c)
	}
	out := f.r.Reconcile(context.Background(), declaredsession.Snapshot{Sessions: []declaredsession.Declaration{f.decl}})
	if out[0].Decision.Wake || out[0].Decision.Reason != "sleep_requested" {
		t.Fatalf("reconcile after a failed sleep = %+v", out[0])
	}
	// An explicit wake clears the intent.
	f.r.Creds = f.store
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	if f.r.sleepRequested("lane1") {
		t.Error("explicit wake must clear the sleep intent")
	}
}

func TestObserveReportsHostPidAndBindsImageLabel(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	obs, _ := f.r.Observe(context.Background(), "lane1")
	if obs.Pid == 0 || obs.Pid != f.eng.containers["acesess-lane1"].pid {
		t.Errorf("pid = %d", obs.Pid)
	}
	// A container whose label claims an image it was not created from.
	f.eng.containers["acesess-lane1"].image = "ghcr.io/aceteam-ai/other:9"
	obs, _ = f.r.Observe(context.Background(), "lane1")
	if !obs.Exists || obs.HasRecord {
		t.Errorf("forged image label accepted: %+v", obs)
	}
}

func TestWakeRefusesNonTmpfsRunBase(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	f.r.IsTmpfs = func(string) (bool, error) { return false, nil }
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrRunDir) {
		t.Fatalf("err = %v", err)
	}
	if len(f.eng.calls) != 0 {
		t.Errorf("engine touched before the tmpfs proof: %v", f.eng.calls)
	}
}

func TestWakeMissingCredentialStartsNothing(t *testing.T) {
	f := newFixture(t, false)
	if err := f.store.Save("lane1", Bundle{"claude-oauth": []byte(oauthSecret)}); err != nil { // no gh-token
		t.Fatal(err)
	}
	_, err := f.r.Wake(context.Background(), f.decl)
	if !errors.Is(err, ErrCredential) || !strings.Contains(err.Error(), "gh-token") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), oauthSecret) {
		t.Fatal("error leaks a secret")
	}
	for _, c := range f.eng.calls {
		if c[0] == "run" || (c[0] == "volume" && c[1] == "create") {
			t.Fatalf("engine mutated despite missing credential: %v", c)
		}
	}
	if HasMaterialized(f.runDir(t)) {
		t.Error("partial secrets left behind")
	}
}

func TestWakeNoSealedBundleRefuses(t *testing.T) {
	f := newFixture(t, false)
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrCredential) {
		t.Fatalf("err = %v", err)
	}
}

func TestWakeReadinessFailureCleansUp(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	f.r.WaitReady = func(context.Context, string, string) error { return errors.New("no hello") }
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrEngine) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := f.eng.containers["acesess-lane1"]; ok {
		t.Error("a container that never became ready was left behind")
	}
	if HasMaterialized(f.runDir(t)) {
		t.Error("secrets left on tmpfs after a failed wake")
	}
	if _, ok := f.eng.volumes["acesess-lane1-home"]; !ok {
		t.Error("a failed wake must never remove the volume")
	}
}

func TestWakeRunFailureWipesSecrets(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	f.eng.failRun = true
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrEngine) {
		t.Fatalf("err = %v", err)
	}
	if HasMaterialized(f.runDir(t)) {
		t.Error("secrets left on tmpfs after run failure")
	}
}

func TestWakeWhileRunningIsRefusedAndHarmless(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	n := len(f.eng.calls)
	if _, err := f.r.Wake(context.Background(), f.decl); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.eng.calls[n:] {
		if c[0] == "run" || c[0] == "rm" || c[0] == "stop" {
			t.Fatalf("a running session was disturbed: %v", c)
		}
	}
}

func TestCrashedWorkerLeftoverSecretsAreResealedBeforeNextWake(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	runDir := f.runDir(t)
	_ = f.eng.harnessWrite(t, "lane1", "/home/agent/.claude/.credentials.json", []byte("ROTATED-BEFORE-CRASH"), true)
	f.eng.containers["acesess-lane1"].state, f.eng.containers["acesess-lane1"].code = "exited", 137
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(runDir, "cfg", ".credentials.json"))
	if string(got) != "ROTATED-BEFORE-CRASH" {
		t.Fatalf("rotation lost across a crash: %q", got)
	}
}

func TestReconcileWakesOnlyInterruptedSessions(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	snap := declaredsession.Snapshot{Sessions: []declaredsession.Declaration{f.decl}}

	// Never started: stays down, nothing created.
	out := f.r.Reconcile(context.Background(), snap)
	if len(out) != 1 || out[0].Decision.Wake || out[0].Decision.Reason != "never_started" {
		t.Fatalf("never started: %+v", out)
	}
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	c := f.eng.containers["acesess-lane1"]

	// Reboot killed it: non-zero exit, woken.
	c.state, c.code = "exited", 137
	out = f.r.Reconcile(context.Background(), snap)
	if !out[0].Decision.Wake || out[0].Err != nil || f.eng.containers["acesess-lane1"].state != "running" {
		t.Fatalf("interrupted: %+v state=%s", out, f.eng.containers["acesess-lane1"].state)
	}
	// Running: untouched.
	n := len(f.eng.calls)
	out = f.r.Reconcile(context.Background(), snap)
	if out[0].Decision.Reason != "already_running" {
		t.Fatalf("running: %+v", out)
	}
	for _, call := range f.eng.calls[n:] {
		if call[0] == "run" || call[0] == "rm" {
			t.Fatalf("running session disturbed: %v", call)
		}
	}
	// Clean harness exit: stays down.
	f.eng.containers["acesess-lane1"].state, f.eng.containers["acesess-lane1"].code = "exited", 0
	if out = f.r.Reconcile(context.Background(), snap); out[0].Decision.Wake || out[0].Decision.Reason != "clean_exit" {
		t.Fatalf("clean exit: %+v", out)
	}
	// Deliberate sleep (container removed, volume kept): stays asleep.
	delete(f.eng.containers, "acesess-lane1")
	if out = f.r.Reconcile(context.Background(), snap); out[0].Decision.Wake || out[0].Decision.Reason != "asleep" {
		t.Fatalf("asleep: %+v", out)
	}
	// An unreadable engine is never read as "nothing there".
	f.eng.failInspect = true
	if out = f.r.Reconcile(context.Background(), snap); out[0].Err == nil || out[0].Decision.Wake {
		t.Fatalf("engine failure: %+v", out)
	}
}

func TestSleepIsIdempotentWhenNothingExists(t *testing.T) {
	f := newFixture(t, false)
	if err := f.r.Sleep(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
}
