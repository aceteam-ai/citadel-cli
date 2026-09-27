package jobs

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

func baseAppInput() AppSpecInput {
	return AppSpecInput{
		ShortCode:     "ac-blue-cat-fox",
		Image:         "ghcr.io/aceteam-ai/streamlit-runtime:latest",
		ContainerPort: 8501,
		Size:          "small",
	}
}

func TestParseAppSpec_Valid(t *testing.T) {
	spec, err := ParseAppSpec(baseAppInput(), 18901, "/home/jason")
	if err != nil {
		t.Fatalf("ParseAppSpec: %v", err)
	}
	if spec.PodName != "aceapp-ac-blue-cat-fox" || spec.ContainerName != "aceapp-ac-blue-cat-fox-app" {
		t.Errorf("names: pod=%q ctr=%q", spec.PodName, spec.ContainerName)
	}
	if spec.HostPort != 18901 || spec.ContainerPort != 8501 {
		t.Errorf("ports: host=%d ctr=%d", spec.HostPort, spec.ContainerPort)
	}
	if spec.HealthPath != "/" {
		t.Errorf("default health path = %q, want /", spec.HealthPath)
	}
	if spec.Limits.CPUs != "1" || spec.Limits.Memory != "1g" || spec.Limits.Pids != 512 {
		t.Errorf("small limits = %+v", spec.Limits)
	}
	// PORT injected from container_port.
	if spec.Env["PORT"] != "8501" {
		t.Errorf("injected PORT = %q, want 8501", spec.Env["PORT"])
	}
	// No state_volume_path -> per-app named volume, removable on destroy.
	if spec.NamedVolume != "aceapp-ac-blue-cat-fox-data" || spec.StateVolume != spec.NamedVolume {
		t.Errorf("named volume = %q, state volume = %q", spec.NamedVolume, spec.StateVolume)
	}
	if spec.StateMountPath != "/data" {
		t.Errorf("mount path = %q, want /data", spec.StateMountPath)
	}
	// Labels are the durable record.
	if spec.Labels[AppLabelShortCode] != "ac-blue-cat-fox" || spec.Labels[AppLabelHostPort] != "18901" {
		t.Errorf("labels = %+v", spec.Labels)
	}
}

func TestParseAppSpec_ExplicitPortWins(t *testing.T) {
	in := baseAppInput()
	in.Env = map[string]string{"PORT": "9000"}
	spec, err := ParseAppSpec(in, 18901, "/home/jason")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Env["PORT"] != "9000" {
		t.Errorf("explicit PORT overridden: %q", spec.Env["PORT"])
	}
}

func TestParseAppSpec_SizeDefaultAndTiers(t *testing.T) {
	for size, want := range map[string]appLimits{
		"":       {CPUs: "1", Memory: "1g", Pids: 512}, // default small
		"nano":   {CPUs: "0.5", Memory: "512m", Pids: 256},
		"medium": {CPUs: "2", Memory: "4g", Pids: 1024},
		"large":  {CPUs: "4", Memory: "8g", Pids: 2048},
		"bogus":  {CPUs: "1", Memory: "1g", Pids: 512}, // unknown falls back to small
	} {
		in := baseAppInput()
		in.Size = size
		spec, err := ParseAppSpec(in, 18901, "/home/jason")
		if err != nil {
			t.Fatalf("size %q: %v", size, err)
		}
		if spec.Limits != want {
			t.Errorf("size %q limits = %+v, want %+v", size, spec.Limits, want)
		}
	}
}

func TestParseAppSpec_Rejects(t *testing.T) {
	t.Run("image not allowlisted", func(t *testing.T) {
		in := baseAppInput()
		in.Image = "docker.io/library/nginx:latest"
		if _, err := ParseAppSpec(in, 18901, "/home/jason"); err == nil {
			t.Error("accepted a non-allowlisted image")
		}
	})
	t.Run("bad runtime", func(t *testing.T) {
		in := baseAppInput()
		in.Runtime = "evil"
		if _, err := ParseAppSpec(in, 18901, "/home/jason"); err == nil {
			t.Error("accepted a non-allowlisted runtime")
		}
	})
	t.Run("privileged container port", func(t *testing.T) {
		in := baseAppInput()
		in.ContainerPort = 80
		if _, err := ParseAppSpec(in, 18901, "/home/jason"); err == nil {
			t.Error("accepted a privileged container port")
		}
	})
	t.Run("bind volume escapes citadel dir", func(t *testing.T) {
		in := baseAppInput()
		in.StateVolumePath = "/etc"
		if _, err := ParseAppSpec(in, 18901, "/home/jason"); err == nil {
			t.Error("accepted a state volume outside the citadel data dir")
		}
	})
}

func TestParseAppSpec_BindVolume(t *testing.T) {
	in := baseAppInput()
	in.StateVolumePath = "~/citadel-cache/apps/x"
	spec, err := ParseAppSpec(in, 18901, "/home/jason")
	if err != nil {
		t.Fatal(err)
	}
	if spec.NamedVolume != "" {
		t.Errorf("bind mount should have no named volume, got %q", spec.NamedVolume)
	}
	if !strings.HasPrefix(spec.StateVolume, "/home/jason/citadel-cache") {
		t.Errorf("bind path = %q", spec.StateVolume)
	}
}

func TestBuildAppContainerRunArgs_Podman(t *testing.T) {
	spec, _ := ParseAppSpec(baseAppInput(), 18901, "/home/jason")
	args := buildAppContainerRunArgs(spec, true)
	joined := strings.Join(args, " ")
	mustContain(t, joined, "--pod aceapp-ac-blue-cat-fox")
	mustContain(t, joined, "--cap-drop ALL")
	mustContain(t, joined, "--cpus 1")
	mustContain(t, joined, "--memory 1g")
	mustContain(t, joined, "--pids-limit 512")
	mustContain(t, joined, "-e PORT=8501")
	// The pod owns the publish on podman; the container must NOT re-publish.
	if strings.Contains(joined, "-p 127.0.0.1") {
		t.Errorf("podman container must not publish (the pod does): %s", joined)
	}
	// Image is LAST so it can never be read as a flag.
	if args[len(args)-1] != spec.Image {
		t.Errorf("image not last: %q", args[len(args)-1])
	}
}

func TestBuildAppContainerRunArgs_Docker(t *testing.T) {
	spec, _ := ParseAppSpec(baseAppInput(), 18901, "/home/jason")
	args := buildAppContainerRunArgs(spec, false)
	joined := strings.Join(args, " ")
	mustContain(t, joined, "--name aceapp-ac-blue-cat-fox")
	mustContain(t, joined, "-p 127.0.0.1:18901:8501")
	mustContain(t, joined, "--cap-drop ALL")
	if strings.Contains(joined, "--pod") {
		t.Errorf("docker has no pods: %s", joined)
	}
	if args[len(args)-1] != spec.Image {
		t.Errorf("image not last: %q", args[len(args)-1])
	}
}

func TestBuildAppPodCreateArgs(t *testing.T) {
	spec, _ := ParseAppSpec(baseAppInput(), 18901, "/home/jason")
	joined := strings.Join(buildAppPodCreateArgs(spec), " ")
	mustContain(t, joined, "--name aceapp-ac-blue-cat-fox")
	mustContain(t, joined, "-p 127.0.0.1:18901:8501")
	mustContain(t, joined, "--label aceteam.app=ac-blue-cat-fox")
}

// --- runner argv (fake exec seam) ------------------------------------------

type fakeExec struct {
	calls   [][]string
	outputs map[string]string // substring -> stdout
}

func (f *fakeExec) run(cmd *exec.Cmd) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), cmd.Args...))
	joined := strings.Join(cmd.Args, " ")
	for sub, out := range f.outputs {
		if strings.Contains(joined, sub) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func (f *fakeExec) argvContains(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return true
		}
	}
	return false
}

// indexOf returns the index of the first call whose joined argv contains sub, or -1.
func (f *fakeExec) indexOf(sub string) int {
	for i, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return i
		}
	}
	return -1
}

func newRunner(engine string, rootless bool, fe *fakeExec) *AppPodRunner {
	rt := catalog.ContainerRuntime{EngineBin: engine, Bin: engine, ComposePrefix: []string{"compose"}, Rootless: rootless}
	return NewAppPodRunner(rt, "/home/jason", nil).WithExec(fe.run)
}

func TestAppPodRunner_DeployPodman(t *testing.T) {
	fe := &fakeExec{outputs: map[string]string{
		"pod inspect":   "podabc\n",
		"image inspect": "sha256:deadbeef\n",
	}}
	// Rootless=false so PreflightLimitControllers is a no-op (hermetic), while
	// EngineBin=podman still exercises the pod path.
	r := newRunner("podman", false, fe)
	spec, _ := ParseAppSpec(baseAppInput(), 18901, "/home/jason")

	res, err := r.Deploy(context.Background(), spec)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.PodID != "podabc" || res.ImageDigest != "sha256:deadbeef" {
		t.Errorf("result = %+v", res)
	}
	// Pull before create; volume created; pod created; container run.
	if !fe.argvContains("pull ghcr.io/aceteam-ai/streamlit-runtime:latest") {
		t.Error("image was not pulled")
	}
	if !fe.argvContains("volume create aceapp-ac-blue-cat-fox-data") {
		t.Error("named volume was not created")
	}
	if !fe.argvContains("pod create --name aceapp-ac-blue-cat-fox") {
		t.Error("pod was not created")
	}
	runIdx := fe.indexOf("run -d --pod")
	if runIdx < 0 {
		t.Fatal("no container run call found")
	}
	if fe.indexOf("pull") > runIdx {
		t.Error("pull must precede run")
	}
	if fe.indexOf("pod create") > runIdx {
		t.Error("pod create must precede container run")
	}
}

func TestAppPodRunner_DeployDocker(t *testing.T) {
	fe := &fakeExec{outputs: map[string]string{
		"ps -a":         "ctr123\n",
		"image inspect": "sha256:cafef00d\n",
	}}
	r := newRunner("docker", false, fe)
	spec, _ := ParseAppSpec(baseAppInput(), 18902, "/home/jason")

	res, err := r.Deploy(context.Background(), spec)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// docker has no pod; pod_id is the container id resolved by label.
	if res.PodID != "ctr123" {
		t.Errorf("docker pod_id = %q, want the container id", res.PodID)
	}
	if fe.argvContains("--pod") {
		t.Error("docker deploy must not use pods")
	}
	if !fe.argvContains("rm -f aceapp-ac-blue-cat-fox") {
		t.Error("stale container was not removed before recreate")
	}
	if !fe.argvContains("run -d --name aceapp-ac-blue-cat-fox -p 127.0.0.1:18902:8501") {
		t.Error("docker run did not publish on the allocated host port")
	}
}

func TestAppPodRunner_StopStartBranch(t *testing.T) {
	// podman uses `pod stop/start`; docker uses `stop/start`.
	fePod := &fakeExec{outputs: map[string]string{"ps -a": "ctr\n"}}
	rp := newRunner("podman", false, fePod)
	if _, err := rp.Stop(context.Background(), "ac-x"); err != nil {
		t.Fatal(err)
	}
	if !fePod.argvContains("pod stop aceapp-ac-x") {
		t.Error("podman stop must use `pod stop`")
	}

	feDoc := &fakeExec{outputs: map[string]string{"ps -a": "ctr\n"}}
	rd := newRunner("docker", false, feDoc)
	if _, err := rd.Start(context.Background(), "ac-x"); err != nil {
		t.Fatal(err)
	}
	if feDoc.argvContains("pod start") || !feDoc.argvContains("start aceapp-ac-x") {
		t.Error("docker start must use `start`, not `pod start`")
	}
}

func TestAppPodRunner_StopNotFound(t *testing.T) {
	fe := &fakeExec{} // ps returns nothing -> not found
	r := newRunner("docker", false, fe)
	state, err := r.Stop(context.Background(), "ac-missing")
	if err != nil {
		t.Fatal(err)
	}
	if state != "not_found" {
		t.Errorf("stop of a missing app = %q, want not_found", state)
	}
}

func TestAppPodRunner_StatusByLabel(t *testing.T) {
	// -a lists it (exists) AND running filter lists it -> running; labels give port/image.
	fe := &fakeExec{outputs: map[string]string{
		"ps -a":                              "ctr42\n",
		"ps --filter":                        "ctr42\n",
		"aceteam.app.host_port":              "18903\n",
		"inspect --format {{.Config.Image}}": "ghcr.io/aceteam-ai/x:1\n",
	}}
	r := newRunner("docker", false, fe)
	info, err := r.Status(context.Background(), "ac-x")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != "running" || info.HostPort != 18903 {
		t.Errorf("status = %+v", info)
	}
}

func TestAppPodRunner_DestroyRemovesVolume(t *testing.T) {
	fe := &fakeExec{}
	r := newRunner("podman", false, fe)
	if err := r.Destroy(context.Background(), "ac-x"); err != nil {
		t.Fatal(err)
	}
	if !fe.argvContains("pod rm -f aceapp-ac-x") {
		t.Error("destroy must remove the pod")
	}
	if !fe.argvContains("volume rm -f aceapp-ac-x-data") {
		t.Error("destroy must remove the named volume")
	}
}

func TestAppPodRunner_ExistingHostPort(t *testing.T) {
	fe := &fakeExec{outputs: map[string]string{
		"ps -a":                 "ctr\n",
		"aceteam.app.host_port": "18905\n",
	}}
	r := newRunner("docker", false, fe)
	p, ok := r.ExistingHostPort(context.Background(), "ac-x")
	if !ok || p != 18905 {
		t.Errorf("ExistingHostPort = (%d,%v), want (18905,true)", p, ok)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected %q to contain %q", haystack, needle)
	}
}
