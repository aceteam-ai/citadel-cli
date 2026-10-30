package containerjobs

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// command runs an argv vector, never a shell. The bounded writer drains excess
// output without growing memory, so a noisy process cannot block on its pipe.
type boundedOutput struct {
	bytes     []byte
	truncated bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := OutputLimit - len(w.bytes)
	if n > left {
		w.truncated = true
		p = p[:left]
	}
	w.bytes = append(w.bytes, p...)
	return n, nil
}

type commandResult struct {
	bytes     []byte
	code      int
	truncated bool
}
type commandFunc func(context.Context, string, ...string) (commandResult, error)

func nativeCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	if bin == "docker" {
		args = append([]string{"--host=unix:///var/run/docker.sock"}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "PATH" || key == "LANG" || key == "LC_ALL" || key == "TZ" || bin == "docker" && (key == "HOME" || key == "DOCKER_CONFIG") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	// A non-nil empty environment must not fall back to inheriting credentials.
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	return cmd
}
func runCommand(ctx context.Context, bin string, args ...string) (commandResult, error) {
	cmd := nativeCommand(ctx, bin, args...)
	out := &boundedOutput{}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		}
	}
	return commandResult{bytes: out.bytes, code: code, truncated: out.truncated}, err
}

var _ io.Writer = (*boundedOutput)(nil)

func createArguments(r Request, runtime string, layerLimit bool) []string {
	args := []string{"create", "--name", r.ContainerRef, "--label", "aceteam.org_id=" + r.OrgID, "--label", "aceteam.volume_ref=" + r.VolumeRef,
		"--cpus", strconv.FormatFloat(r.CPUs, 'f', -1, 64), "--memory", strconv.FormatInt(r.MemoryBytes, 10), "--memory-swap", strconv.FormatInt(r.MemoryBytes, 10),
		"--pids-limit", strconv.FormatInt(r.PidsLimit, 10), "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--mount", "type=volume,src=" + r.VolumeRef + ",dst=/workspace", "--workdir", "/workspace"}
	if runtime != "" {
		args = append(args, "--runtime", runtime)
	}
	if layerLimit {
		args = append(args, "--storage-opt", "size=4G")
	}
	network := r.Network
	if network == "" {
		network = "none"
	}
	args = append(args, "--network", network, r.Image)
	return args
}
