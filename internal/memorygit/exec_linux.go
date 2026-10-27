package memorygit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func validAbsolutePath(path string) bool { return filepath.IsAbs(path) && filepath.Clean(path) == path }

// This first slice requires a private, node-owned repository on Linux. The
// pinned directory descriptor is inherited by Git, closing root rename/swap
// races. Git never opens a working-tree file. Imported/untrusted repos are out
// of scope; no clone/init/repair is performed by a remote request.
func openRepository(ctx context.Context, path string) (*os.File, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, failure("repository_unavailable")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, failure("repository_unavailable")
	}
	defer root.Close()
	info, err := root.Lstat(".git")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, failure("repository_unavailable")
	}
	gitDir, err := root.Open(".git")
	if err != nil {
		return nil, failure("repository_unavailable")
	}
	good := false
	defer func() {
		if !good {
			gitDir.Close()
		}
	}()
	// A dedicated .git directory and every metadata entry must be owned by
	// this node uid, unwritable by others and free of links/special files.
	// The bounded walk also rules out linked-worktree/alternate object roots.
	gitRoot, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", gitDir.Fd()))
	if err != nil {
		return nil, failure("repository_unavailable")
	}
	defer gitRoot.Close()
	entries := 0
	var walk func(string, int) error
	walk = func(name string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > 16384 || depth > 32 {
			return failure("repository_unavailable")
		}
		if name == "commondir" || name == "gitdir" || name == "objects/info/alternates" || name == "objects/info/http-alternates" || name == "info/grafts" {
			return failure("repository_unavailable")
		}
		info, err := gitRoot.Lstat(name)
		if err != nil {
			return failure("repository_unavailable")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 || (!info.IsDir() && !info.Mode().IsRegular()) || (info.Mode().IsRegular() && stat.Nlink != 1) {
			return failure("repository_unavailable")
		}
		if name == "." && info.Mode().Perm()&0077 != 0 {
			return failure("repository_unavailable")
		}
		if !info.IsDir() {
			return nil
		}
		dir, err := gitRoot.Open(name)
		if err != nil {
			return failure("repository_unavailable")
		}
		defer dir.Close()
		for {
			children, readErr := dir.ReadDir(128)
			for _, child := range children {
				path := child.Name()
				if name != "." {
					path = name + "/" + path
				}
				if err := walk(path, depth+1); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return failure("repository_unavailable")
			}
		}
	}
	err = walk(".", 0)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure("repository_unavailable")
	}
	good = true
	return gitDir, nil
}

var errOutputLimit = errors.New("output limit")

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > MaxOutputBytes-b.Len() {
		return 0, errOutputLimit
	}
	return b.Buffer.Write(p)
}

func runGit(ctx context.Context, gitPath string, repo *os.File, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	argv := []string{"--no-pager", "--no-optional-locks", "--literal-pathspecs", "--bare", "--git-dir=/proc/self/fd/3",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null",
		"-c", "protocol.allow=never", "-c", "log.showSignature=false"}
	cmd := exec.CommandContext(childCtx, gitPath, append(argv, args...)...)
	cmd.ExtraFiles = []*os.File{repo}
	cmd.Dir = "/"
	cmd.WaitDelay = time.Second
	// No inherited GIT_*, loader variables, credentials, HOME config, pager,
	// PATH lookup or caller-provided environment enters the child.
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_ATTR_NOSYSTEM=1"}
	var out limitedOutput
	var exceeded bool
	// stdout/stderr may be copied concurrently. Stderr is discarded and never
	// shares this writer. An overflow immediately cancels the process.
	cmd.Stdout = writerFunc(func(p []byte) (int, error) {
		n, err := out.Write(p)
		if err != nil {
			exceeded = true
			cancel()
		}
		return n, err
	})
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if exceeded {
		return nil, failure("output_limit")
	}
	if err != nil {
		return nil, failure("git_failed")
	}
	return out.Bytes(), nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
