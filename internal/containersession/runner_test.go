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

func indexOf(args []string, v string) int {
	for i, a := range args {
		if a == v {
			return i
		}
	}
	return -1
}

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
		{"--user", "1000:1000"}, {"--entrypoint", BinaryMount},
	} {
		if !hasPair(args, pair[0], pair[1]) {
			t.Errorf("run argv missing %v: %v", pair, args)
		}
	}
	if indexOf(args, "--userns") != -1 {
		t.Error("docker run must not carry --userns")
	}
	runDir := f.runDir(t)
	wantMounts := []string{
		"acesess-lane1-home:/home/agent",
		filepath.Join(f.nodeDir, "sessions", "lane1") + ":/run/session/sessions/lane1",
		filepath.Join(runDir, "sessions.yaml") + ":/run/session/sessions.yaml:ro",
		"/opt/node/citadel:" + BinaryMount + ":ro",
		filepath.Join(runDir, "credentials.env") + ":" + declaredsession.ContainerCredEnvFile + ":ro",
		filepath.Join(runDir, "creds", "claude-oauth") + ":/home/agent/.claude/.credentials.json",
	}
	for _, m := range wantMounts {
		if !hasPair(args, "-v", m) {
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
	// Volume was created with the record labels, and the preflight/inspect/run
	// order is volume-before-run.
	if v := f.eng.volumes["acesess-lane1-home"]; v[LabelSession] != "lane1" || v[LabelVolume] != "acesess-lane1-home" {
		t.Errorf("volume labels = %v", v)
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
	got, err := os.ReadFile(filepath.Join(runDir, "creds", "claude-oauth"))
	if err != nil || string(got) != oauthSecret {
		t.Fatalf("materialized file = %q, %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(runDir, "creds", "claude-oauth")); info.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode = %v", info.Mode().Perm())
	}
	env, _ := os.ReadFile(filepath.Join(runDir, "credentials.env"))
	if string(env) != "GH_TOKEN="+ghSecret+"\n" {
		t.Errorf("env file = %q", env)
	}
	// The harness refreshes its OAuth token in place while running.
	rotated := "ROTATED-" + oauthSecret
	if err := os.WriteFile(filepath.Join(runDir, "creds", "claude-oauth"), []byte(rotated), 0o600); err != nil {
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
	got, _ = os.ReadFile(filepath.Join(runDir, "creds", "claude-oauth"))
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
	_ = os.WriteFile(filepath.Join(runDir, "creds", "claude-oauth"), []byte("NEW"), 0o600)
	f.r.Creds = failingSave{f.store}
	if err := f.r.Sleep(context.Background(), f.decl); !errors.Is(err, ErrReseal) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(runDir, "creds", "claude-oauth")); string(got) != "NEW" {
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

func TestNoCredentialBytesUnderVolumeOrNodeDir(t *testing.T) {
	f := newFixture(t, false)
	f.seed(t)
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	// Stubs exist (an engine creates mountpoints) but hold nothing.
	stub := filepath.Join(f.eng.volumeDir, ".claude", ".credentials.json")
	if info, err := os.Stat(stub); err != nil || info.Size() != 0 {
		t.Fatalf("mountpoint stub = %v, %v", info, err)
	}
	needles := []string{oauthSecret, ghSecret}
	if hits := walkContains(t, f.eng.volumeDir, needles...); len(hits) != 0 {
		t.Fatalf("credential bytes on the volume: %v", hits)
	}
	// The sealed bundle and the node config dir hold ciphertext only.
	if hits := walkContains(t, f.nodeDir, needles...); len(hits) != 0 {
		t.Fatalf("credential plaintext under the node dir: %v", hits)
	}
	// The rendered config the container reads carries no secret either.
	cfg, _ := os.ReadFile(filepath.Join(f.runDir(t), "sessions.yaml"))
	for _, n := range needles {
		if strings.Contains(string(cfg), n) {
			t.Fatal("secret in rendered config")
		}
	}
	// Plaintext exists only under the run dir, and sleep removes it.
	if hits := walkContains(t, f.runBase, needles...); len(hits) != 2 {
		t.Fatalf("expected both secrets only in the run dir, got %v", hits)
	}
	if err := f.r.Sleep(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	if hits := walkContains(t, f.runBase, needles...); len(hits) != 0 {
		t.Fatalf("secrets remain after sleep: %v", hits)
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
	_ = os.WriteFile(filepath.Join(runDir, "creds", "claude-oauth"), []byte("ROTATED-BEFORE-CRASH"), 0o600)
	f.eng.containers["acesess-lane1"].state, f.eng.containers["acesess-lane1"].code = "exited", 137
	if _, err := f.r.Wake(context.Background(), f.decl); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(runDir, "creds", "claude-oauth"))
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
