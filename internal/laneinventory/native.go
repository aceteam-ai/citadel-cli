package laneinventory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/aceteam-ai/citadel-cli/internal/procscope"
	"github.com/aceteam-ai/citadel-cli/internal/tmux"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

const maxProbeBytes = 1 << 20
const paneFormat = "#{session_name}\x1f#{window_id}\x1f#{pane_id}\x1f#{pane_current_command}\x1f#{pane_current_path}\x1f#{pane_pid}\x1f#{@citadel_managed}"

type commandRunner func(context.Context, string, ...string) ([]byte, error)

// These narrow seams exercise the real adapter policy with fake observations,
// without enumerating processes or opening a real cmdline in source tests.
type processReader interface {
	UidsWithContext(context.Context) ([]int32, error)
	PpidWithContext(context.Context) (int32, error)
	CreateTimeWithContext(context.Context) (int64, error)
}

type processRef struct {
	pid    int32
	reader processReader
}

func ownProcessUID(ids []int32, uid int32) bool {
	return uid >= 0 && len(ids) >= 2 && ids[0] == uid && ids[1] == uid
}

func nativeProcesses(ctx context.Context, uid int32, list func(context.Context) ([]processRef, error)) ([]Process, error) {
	processes, err := list(ctx)
	if err != nil {
		return nil, ErrUnavailable
	}
	out := []Process{}
	for _, p := range processes {
		if ctx.Err() != nil {
			return nil, ErrUnavailable
		}
		ids, err := p.reader.UidsWithContext(ctx)
		if err != nil || !ownProcessUID(ids, uid) {
			continue
		}
		parent, err := p.reader.PpidWithContext(ctx)
		if err != nil {
			continue
		}
		started, _ := p.reader.CreateTimeWithContext(ctx)
		// Preserve the observation, not the expected UID. The collector checks
		// it independently before walking descendants or projecting a pane.
		out = append(out, Process{PID: p.pid, Parent: parent, UID: ids[0], StartedAt: started})
		if len(out) > MaxProcesses {
			break
		}
	}
	return out, nil
}

func nativeArgv(ctx context.Context, uid int32, identity func(context.Context) ([]int32, error), read func(context.Context) ([]string, error)) ([]string, error) {
	ids, err := identity(ctx)
	if err != nil || !ownProcessUID(ids, uid) || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	return read(ctx)
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, ErrUnavailable
	}
	return b.Buffer.Write(p)
}

// runReadOnly uses fixed argv only, never a shell, and never returns stderr.
// Git's inherited override/config variables and optional mutating helpers are
// excluded. No credential environment is passed to these local readers.
func runReadOnly(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = probeEnv(os.LookupEnv)
	output := &cappedBuffer{limit: maxProbeBytes}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return nil, ErrUnavailable
	}
	return output.Bytes(), nil
}

func probeEnv(lookup func(string) (string, bool)) []string {
	var env []string
	for _, key := range []string{"PATH", "HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME"} {
		if value, ok := lookup(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

func parsePanes(raw []byte) ([]RawPane, error) {
	return parsePanesForBase(raw, "citadel")
}

func parsePanesForBase(raw []byte, terminalBase string) ([]RawPane, error) {
	if len(raw) > maxProbeBytes {
		return nil, ErrUnavailable
	}
	panes := []RawPane{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x1f")
		if len(fields) != 7 || !strings.HasPrefix(fields[1], "@") || !strings.HasPrefix(fields[2], "%") {
			return nil, ErrUnavailable
		}
		pid, err := strconv.ParseInt(fields[5], 10, 32)
		if err != nil || pid <= 0 {
			return nil, ErrUnavailable
		}
		// Marker handles arbitrary configured/overridden terminal names. Name
		// prefixes conservatively cover legacy unmarked default sessions too.
		terminal := fields[6] != "" || terminalSessionName(fields[0], "citadel") || terminalSessionName(fields[0], terminalBase)
		panes = append(panes, RawPane{Session: fields[0], Window: fields[1], Target: fields[2], Command: fields[3], Cwd: fields[4], PID: int32(pid), Terminal: terminal})
		if len(panes) > MaxPanes {
			break
		}
	}
	return panes, nil
}

func terminalSessionName(name, base string) bool {
	return base != "" && (name == base || strings.HasPrefix(name, base+"-"))
}

// NewNative does not perform any observation at construction. The caller
// selects nodeDir once at the CLI/worker boundary; no leaf home/config lookup.
// Unsupported same-user process identity (notably Windows) refuses rather than
// returning another user's data. No Environ, prompt/rollout/log read exists.
func NewNative(nodeDir string) Collector {
	uid := int32(os.Geteuid())
	return Collector{Deps: Deps{
		UID: uid,
		Panes: func(ctx context.Context) ([]RawPane, error) {
			if runtime.GOOS == "windows" || os.Getuid() != os.Geteuid() {
				return nil, ErrUnavailable
			}
			// tmux -N forbids launching a new server on an absent socket.
			binary, err := tmux.Resolve()
			if err != nil {
				return nil, ErrUnavailable
			}
			raw, err := runReadOnly(ctx, binary, "-N", "list-panes", "-a", "-F", paneFormat)
			if err != nil {
				return nil, err
			}
			return parsePanesForBase(raw, os.Getenv("CITADEL_TERMINAL_SESSION"))
		},
		Processes: func(ctx context.Context) ([]Process, error) {
			return nativeProcesses(ctx, uid, func(ctx context.Context) ([]processRef, error) {
				list, err := process.ProcessesWithContext(ctx)
				out := make([]processRef, 0, len(list))
				for _, p := range list {
					out = append(out, processRef{pid: p.Pid, reader: p})
				}
				return out, err
			})
		},
		Argv: func(ctx context.Context, pid int32) ([]string, error) {
			p, err := process.NewProcessWithContext(ctx, pid)
			if err != nil {
				return nil, ErrUnavailable
			}
			return nativeArgv(ctx, uid, p.UidsWithContext, func(ctx context.Context) ([]string, error) {
				// Linux cmdline is bounded before allocation. Other Unix systems use
				// their process API; the collector validates argc and byte bounds too.
				if runtime.GOOS == "linux" {
					file, err := os.Open("/proc/" + strconv.Itoa(int(pid)) + "/cmdline")
					if err != nil {
						return nil, ErrUnavailable
					}
					defer file.Close()
					raw, err := io.ReadAll(io.LimitReader(file, MaxArgBytes+1))
					if err != nil || len(raw) > MaxArgBytes {
						return nil, ErrUnavailable
					}
					return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), nil
				}
				return p.CmdlineSliceWithContext(ctx)
			})
		},
		Declarations: func(ctx context.Context) (map[int32]string, error) { return declaredPIDs(ctx, nodeDir) },
		Git:          func(ctx context.Context, cwd string) (*Worktree, error) { return readGit(ctx, runReadOnly, cwd) },
		Capacity: func(ctx context.Context) (Capacity, error) {
			value := Capacity{NCPU: runtime.NumCPU()}
			var failures []error
			if v, err := load.AvgWithContext(ctx); err == nil {
				value.Load1 = &v.Load1
				value.Load5 = &v.Load5
			} else {
				failures = append(failures, ErrUnavailable)
			}
			if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
				value.MemoryAvailable = &v.Available
			} else {
				failures = append(failures, ErrUnavailable)
			}
			if filepath.IsAbs(nodeDir) {
				if v, err := disk.UsageWithContext(ctx, nodeDir); err == nil {
					value.DiskAvailable = &v.Free
				} else {
					failures = append(failures, ErrUnavailable)
				}
			} else {
				failures = append(failures, ErrUnavailable)
			}
			return value, errors.Join(failures...)
		},
	}}
}

func declaredPIDs(ctx context.Context, nodeDir string) (map[int32]string, error) {
	store, err := declaredsession.OpenStore(nodeDir)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer store.Close()
	config, err := store.LoadConfig()
	if err != nil {
		return nil, ErrUnavailable
	}
	result := map[int32]string{}
	for _, declaration := range config.Sessions {
		if ctx.Err() != nil {
			return nil, ErrUnavailable
		}
		session, err := store.OpenSession(declaration.Name, false)
		if err != nil {
			continue
		}
		state, present, err := session.ReadState()
		_ = session.Close()
		if err != nil || !present || state.Phase == "stopped" {
			continue
		}
		for _, expected := range []declaredsession.ProcessIdentity{state.Holder, state.Child} {
			if expected.PID <= 0 {
				continue
			}
			actual, err := procscope.ObserveProcess(expected.PID)
			if err == nil && matchesDeclaredIdentity(expected, actual) {
				result[int32(expected.PID)] = declaration.Name
			}
		}
	}
	return result, nil
}

func matchesDeclaredIdentity(expected declaredsession.ProcessIdentity, actual procscope.ProcessIdentity) bool {
	return expected.PID > 0 && expected.StartTicks > 0 && expected.UID > 0 && expected.BootID != "" && actual.PID == expected.PID && actual.StartTicks == expected.StartTicks && actual.UID == expected.UID && actual.BootID == expected.BootID
}

func readGit(ctx context.Context, run commandRunner, cwd string) (*Worktree, error) {
	if !filepath.IsAbs(cwd) {
		return nil, ErrUnavailable
	}
	git := func(args ...string) ([]byte, error) {
		return run(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-C", cwd}, args...)...)
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, ErrUnavailable
	}
	head, err := git("rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, ErrUnavailable
	}
	branch, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, ErrUnavailable
	}
	remotes, err := git("remote")
	if err != nil {
		return nil, ErrUnavailable
	}
	status, err := git("status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		return nil, ErrUnavailable
	}
	dirty, err := dirtyCount(status)
	if err != nil {
		return nil, err
	}
	w := &Worktree{Path: strings.TrimSpace(string(root)), Head: strings.TrimSpace(string(head)), Branch: strings.TrimSpace(string(branch)), Remotes: strings.Fields(string(remotes)), DirtyFiles: dirty}
	w.Detached = w.Branch == "HEAD"
	if w.Detached {
		w.Branch = ""
	}
	if ahead, err := git("rev-list", "--count", "@{upstream}..HEAD"); err == nil {
		if count, err := strconv.ParseUint(strings.TrimSpace(string(ahead)), 10, 64); err == nil {
			w.Ahead = &count
		}
	}
	return w, nil
}

func dirtyCount(raw []byte) (uint64, error) {
	if len(raw) > maxProbeBytes || len(raw) > 0 && raw[len(raw)-1] != 0 {
		return 0, ErrUnavailable
	}
	entries := bytes.Split(raw, []byte{0})
	var count uint64
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) == 0 {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return 0, ErrUnavailable
		}
		count++
		if entry[0] == 'R' || entry[1] == 'R' || entry[0] == 'C' || entry[1] == 'C' {
			i++
			if i >= len(entries) || len(entries[i]) == 0 {
				return 0, ErrUnavailable
			}
		}
	}
	return count, nil
}
