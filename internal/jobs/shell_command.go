// internal/jobs/shell_command.go
package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// standardPATHDirs are directories ensured on PATH when the inherited process
// environment is restricted (e.g. citadel running via systemd or nohup with a
// minimal PATH). They are merged into the command environment so /bin/sh can
// resolve common executables.
var standardPATHDirs = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
}

// augmentedEnv returns os.Environ() with the PATH entry extended to include any
// standardPATHDirs not already present (no duplicates). If no PATH entry exists,
// one is added. This preserves the restricted-environment fallback from #154
// now that commands run through /bin/sh rather than a direct exec lookup.
func augmentedEnv() []string {
	env := os.Environ()
	sep := string(os.PathListSeparator)
	for i, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			continue
		}
		current := strings.TrimPrefix(kv, "PATH=")
		existing := make(map[string]struct{})
		for _, d := range filepath.SplitList(current) {
			existing[d] = struct{}{}
		}
		var additions []string
		for _, d := range standardPATHDirs {
			if _, ok := existing[d]; !ok {
				additions = append(additions, d)
			}
		}
		if len(additions) > 0 {
			env[i] = "PATH=" + current + sep + strings.Join(additions, sep)
		}
		return env
	}
	return append(env, "PATH="+strings.Join(standardPATHDirs, sep))
}

// ShellCommandHandler executes a command string through /bin/sh -c, so pipes,
// redirects, && / ||, quoting, and command substitution behave as expected.
type ShellCommandHandler struct {
	// WorkspaceDir, when non-empty, is set as the command's working directory so
	// relative paths resolve consistently with the file-operation handlers.
	WorkspaceDir string
}

// NewShellCommandHandler constructs a handler bound to a workspace directory.
// A zero-value &ShellCommandHandler{} remains valid (no working directory set).
func NewShellCommandHandler(workspace string) *ShellCommandHandler {
	return &ShellCommandHandler{WorkspaceDir: workspace}
}

func (h *ShellCommandHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	cmdString, ok := job.Payload["command"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'command' field")
	}
	if strings.TrimSpace(cmdString) == "" {
		return nil, fmt.Errorf("empty command")
	}
	ctx.Log("info", "     - [Job %s] Running shell command: '%s'", job.ID, cmdString)

	cmd := exec.Command("/bin/sh", "-c", cmdString)
	cmd.Env = augmentedEnv()
	if h.WorkspaceDir != "" {
		cmd.Dir = h.WorkspaceDir
	}
	return cmd.CombinedOutput()
}
