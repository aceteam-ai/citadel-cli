package containerjobs

import (
	"context"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// Host storage is independent of any invoker-writable node configuration.
	HostRoot         = "/var/lib/citadel/container-volumes"
	WorkspaceUID     = 10001
	WorkspaceGID     = 10001
	proxyHostMapping = "host.docker.internal:host-gateway"
	proxyEndpoint    = "host.docker.internal:3128"
	proxyNoProxy     = "localhost,127.0.0.1,::1"
)

// HostPolicy is read from the node, not a container request. An empty proxy URL
// adds no proxy environment or host-gateway mapping. In particular, job fields
// and the worker's ambient HTTP_PROXY environment cannot select this policy.
type HostPolicy struct {
	Runtime        string
	EgressProxyURL string
	Limits         HostLimits
}

func ValidEgressProxyURL(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\t #") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host != proxyEndpoint ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return false
	}
	return true
}

func (p HostPolicy) validate() error {
	if (p.Runtime != "" && p.Runtime != "runsc") || !ValidEgressProxyURL(p.EgressProxyURL) {
		return ErrInvalid
	}
	return nil
}

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
	program := nativeProgramPath(bin)
	if program == "" {
		return nil
	}
	if bin == "docker" {
		args = append([]string{"--host=unix:///var/run/docker.sock"}, args...)
	}
	cmd := exec.CommandContext(ctx, program, args...)
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
	if cmd == nil {
		return commandResult{}, ErrUnavailable
	}
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

func createArguments(r Request, policy HostPolicy, layerLimit bool) ([]string, error) {
	if r.Network != "" && r.Network != "none" {
		return nil, ErrInvalid
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if !layerLimit {
		return nil, ErrUnavailable
	}
	args := []string{"create", "--name", r.ContainerRef, "--label", "aceteam.org_id=" + r.OrgID, "--label", "aceteam.volume_ref=" + r.VolumeRef,
		"--cpus", strconv.FormatFloat(r.CPUs, 'f', -1, 64), "--memory", strconv.FormatInt(r.MemoryBytes, 10), "--memory-swap", strconv.FormatInt(r.MemoryBytes, 10),
		"--pids-limit", strconv.FormatInt(r.PidsLimit, 10), "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--mount", "type=volume,src=" + r.VolumeRef + ",dst=/workspace", "--workdir", "/workspace"}
	if policy.Runtime != "" {
		args = append(args, "--runtime", policy.Runtime)
	}
	if layerLimit {
		args = append(args, "--storage-opt", "size=4G")
	}
	if policy.EgressProxyURL != "" {
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			args = append(args, "--env", key+"="+policy.EgressProxyURL)
		}
		for _, key := range []string{"NO_PROXY", "no_proxy"} {
			args = append(args, "--env", key+"="+proxyNoProxy)
		}
		args = append(args, "--add-host", proxyHostMapping)
	}
	network := r.Network
	if network == "" {
		network = "none"
	}
	args = append(args, "--network", network, r.Image)
	return args, nil
}
