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

// standardPATHDirs are directories to search when the process PATH
// does not include them (e.g. when citadel runs via systemd or nohup
// with a restricted environment).
var standardPATHDirs = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
}

// resolveExecutable finds the absolute path of an executable.
// It first tries the normal PATH lookup (exec.LookPath). If that fails,
// it falls back to searching standardPATHDirs for the binary.
func resolveExecutable(name string) (string, error) {
	// If the name contains a slash it is already a path; skip lookup.
	if strings.Contains(name, "/") {
		return name, nil
	}

	// Try the current process PATH first (preserves existing behavior).
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}

	// Fallback: scan standard directories that may not be in PATH.
	for _, dir := range standardPATHDirs {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("executable %q not found in PATH or standard system directories", name)
}

type ShellCommandHandler struct{}

func (h *ShellCommandHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	cmdString, ok := job.Payload["command"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'command' field")
	}
	ctx.Log("info", "     - [Job %s] Running shell command: '%s'", job.ID, cmdString)
	parts := strings.Fields(cmdString)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	resolvedPath, err := resolveExecutable(parts[0])
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(resolvedPath, parts[1:]...)
	return cmd.CombinedOutput()
}
