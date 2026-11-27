// Package laneinventory projects bounded, read-only lane metadata. Native
// observation is behind explicit seams; raw argv and git status never serialize.
package laneinventory

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/aceteam-ai/citadel-cli/internal/memory"
	"github.com/aceteam-ai/citadel-cli/internal/redact"
	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

const (
	MaxBytes       = 64 << 10
	MaxPanes       = 128
	MaxProcesses   = 4096
	MaxArgBytes    = 32 << 10
	maxAllArgBytes = 8 << 20
	maxSecretBytes = 64 << 10
	maxRemotes     = 32
	maxRemoteBytes = 128
	Timeout        = 15 * time.Second
)

var ErrUnavailable = errors.New("lane inventory observation unavailable")

type RawPane struct {
	Session, Window, Target, Command, Cwd string
	PID                                   int32
	ServerPID                             int32
	TTY                                   string
	// Terminal marks a managed/recognized web terminal. No authenticated
	// caller identity currently reaches this collector, so omit it entirely.
	Terminal bool
}

type Process struct {
	PID, Parent, UID int32
	StartedAt        int64 // Unix milliseconds, matching the process observer.
	TTY              string
}

type Worktree struct {
	Path       string   `json:"path"`
	Remotes    []string `json:"remotes"`
	Branch     string   `json:"branch,omitempty"`
	Head       string   `json:"head"`
	Detached   bool     `json:"detached"`
	Ahead      *uint64  `json:"ahead,omitempty"` // nil means no upstream or unobserved.
	DirtyFiles uint64   `json:"dirty_files"`
}

type Capacity struct {
	Load1           *float64 `json:"load1,omitempty"`
	Load5           *float64 `json:"load5,omitempty"`
	NCPU            int      `json:"ncpu"`
	MemoryAvailable *uint64  `json:"memory_available_bytes,omitempty"`
	DiskAvailable   *uint64  `json:"disk_available_bytes,omitempty"`
}

type Pane struct {
	Session         string    `json:"session"`
	Window          string    `json:"window"`
	Target          string    `json:"target"`
	Command         string    `json:"command"`
	Cwd             string    `json:"cwd"`
	StartedAt       *int64    `json:"started_at_ms,omitempty"`
	DeclaredSession string    `json:"declared_session,omitempty"`
	Harness         string    `json:"harness"`
	ThreadID        string    `json:"thread_id,omitempty"`
	Worktree        *Worktree `json:"worktree,omitempty"`
}

type Report struct {
	Panes       []Pane   `json:"panes"`
	Capacity    Capacity `json:"capacity"`
	Unavailable []string `json:"unavailable,omitempty"` // finite component names, never errors.
	Truncated   bool     `json:"truncated"`
}

// Declarations maps currently verified holder/child PIDs to declared names.
// This is only an observation, never authority to attach, adopt or signal.
type Deps struct {
	UID          int32
	Panes        func(context.Context) ([]RawPane, error)
	Processes    func(context.Context) ([]Process, error)
	Argv         func(context.Context, Process) ([]string, error)
	ThreadID     func(context.Context, Process, string) (string, error)
	Declarations func(context.Context) (map[int32]string, error)
	Git          func(context.Context, string) (*Worktree, error)
	Capacity     func(context.Context) (Capacity, error)
}

type Collector struct{ Deps Deps }

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var gitHead = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// Detect recognizes the invoked program, not arbitrary prose mentioning one.
// Node-based launchers are recognized only at argv[1]. No prompt text returns.
func Detect(argv []string) (string, string) {
	if len(argv) == 0 {
		return "other", ""
	}
	index := 0
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[index])), ".exe")
	if (base == "node" || base == "nodejs") && len(argv) > 1 {
		index++
		base = strings.TrimSuffix(strings.ToLower(filepath.Base(argv[index])), ".js")
		if strings.Contains(filepath.ToSlash(argv[index]), "/@anthropic-ai/claude-code/") && base == "cli" {
			base = "claude"
		}
		if strings.Contains(filepath.ToSlash(argv[index]), "/@qwen-code/qwen-code/") && base == "cli" {
			base = "qwen"
		}
	}
	if base != "codex" && base != "claude" && base != "qwen" && base != "ace" {
		return "other", ""
	}
	thread := ""
	for i := index + 1; i < len(argv); i++ {
		arg := argv[i]
		if base == "codex" && (arg == "resume" || arg == "fork") && i+1 < len(argv) && uuid.MatchString(argv[i+1]) {
			thread = argv[i+1]
			break
		}
		if base == "claude" {
			for _, option := range []string{"--resume", "--session-id"} {
				if arg == option && i+1 < len(argv) && uuid.MatchString(argv[i+1]) {
					thread = argv[i+1]
				}
				if value, ok := strings.CutPrefix(arg, option+"="); ok && uuid.MatchString(value) {
					thread = value
				}
			}
		}
	}
	return base, thread
}

func argvSecrets(argv []string) []string {
	var result []string
	for i, arg := range argv {
		if key, value, ok := strings.Cut(arg, "="); ok && redact.IsSensitiveName(key) && key != "--session-id" && value != "" {
			result = append(result, value)
		}
		if strings.HasPrefix(arg, "-") && redact.IsSensitiveName(arg) && arg != "--session-id" && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
			result = append(result, argv[i+1])
		}
	}
	return result
}

func clean(text string, secrets []string) string {
	text = memory.RedactSensitiveText(text, secrets...)
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	text = memory.RedactSensitiveText(text)
	if len(trust.CheckSecrets("", text)) > 0 {
		return "[REDACTED]"
	}
	if strings.Contains(text, "://") {
		return "[OMITTED: URL]"
	}
	if len(text) > 4096 {
		return "[OMITTED: field limit]"
	}
	return text
}

// Collect never echoes a probe error, argv, status filename, environment or URL.
// Partial observations are marked unavailable rather than silently empty.
func (c Collector) Collect(ctx context.Context) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	d := c.Deps
	r := Report{Panes: []Pane{}}
	if ctx.Err() != nil || d.UID < 0 || d.Panes == nil || d.Processes == nil || d.Argv == nil {
		return r, ErrUnavailable
	}
	panes, err := d.Panes(ctx)
	if err != nil {
		return r, ErrUnavailable
	}
	procs, err := d.Processes(ctx)
	if err != nil {
		r.Unavailable = append(r.Unavailable, "processes")
	}
	if len(procs) > MaxProcesses {
		procs = procs[:MaxProcesses]
		r.Truncated = true
	}
	byPID := map[int32]Process{}
	children := map[int32][]int32{}
	for _, p := range procs {
		if p.UID == d.UID && p.PID > 0 {
			byPID[p.PID] = p
			children[p.Parent] = append(children[p.Parent], p.PID)
		}
	}
	for _, ids := range children {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	blocked := map[int32]bool{}
	// The serving process can itself live below an operator pane. Never walk
	// into it: its bare PTY children need not have a separate tmux pane/marker.
	blockedQueue := []int32{int32(os.Getpid())}
	for _, pane := range panes {
		if pane.Terminal {
			blockedQueue = append(blockedQueue, pane.PID)
			r.Unavailable = append(r.Unavailable, "terminal_scope")
		}
	}
	for len(blockedQueue) > 0 {
		pid := blockedQueue[0]
		blockedQueue = blockedQueue[1:]
		if blocked[pid] {
			continue
		}
		blocked[pid] = true
		blockedQueue = append(blockedQueue, children[pid]...)
	}
	// All terminal roots must be blocked before the display-window cap.
	if len(panes) > MaxPanes {
		panes = panes[:MaxPanes]
		r.Truncated = true
	}
	declarations := map[int32]string{}
	if d.Declarations != nil {
		declarations, err = d.Declarations(ctx)
	} else {
		err = ErrUnavailable
	}
	if err != nil {
		r.Unavailable = append(r.Unavailable, "declarations")
	}
	if d.Capacity != nil {
		r.Capacity, err = d.Capacity(ctx)
	} else {
		err = ErrUnavailable
	}
	if err != nil {
		r.Unavailable = append(r.Unavailable, "capacity")
	}
	var secrets []string
	allArgBytes, secretBytes := 0, 0
	argvCache := map[int32][]string{}
	gitCache := map[string]*Worktree{}
	fdProcesses := 0
	for _, raw := range panes {
		if ctx.Err() != nil {
			return Report{}, ErrUnavailable
		}
		if raw.Terminal || blocked[raw.PID] {
			continue
		}
		p := Pane{Session: raw.Session, Window: raw.Window, Target: raw.Target, Command: filepath.Base(raw.Command), Cwd: raw.Cwd, Harness: "other"}
		root, own := byPID[raw.PID]
		if !own || raw.ServerPID <= 0 || root.Parent != raw.ServerPID || raw.TTY == "" || root.TTY != raw.TTY {
			r.Unavailable = append(r.Unavailable, "pane_process")
			continue
		}
		if root.StartedAt > 0 {
			value := root.StartedAt
			p.StartedAt = &value
		}
		queue := []int32{raw.PID}
		seen := map[int32]bool{}
		harnessTree := map[int32]bool{}
		fdThreads := map[string]bool{}
		fdFailed := false
		for len(queue) > 0 {
			if ctx.Err() != nil {
				return Report{}, ErrUnavailable
			}
			pid := queue[0]
			queue = queue[1:]
			observed, present := byPID[pid]
			if seen[pid] || blocked[pid] || !present || observed.TTY != raw.TTY {
				continue
			}
			seen[pid] = true
			if name := declarations[pid]; name != "" {
				p.DeclaredSession = name
			}
			args, cached := argvCache[pid]
			if !cached {
				args, err = d.Argv(ctx, observed)
				total := 0
				for _, arg := range args {
					total += len(arg)
				}
				allArgBytes += total
				if allArgBytes > maxAllArgBytes {
					return Report{}, ErrUnavailable
				}
				if err != nil || len(args) > 256 || total > MaxArgBytes {
					args = nil
					r.Unavailable = append(r.Unavailable, "argv")
				}
				argvCache[pid] = args
				for _, value := range argvSecrets(args) {
					secretBytes += len(value)
					if secretBytes > maxSecretBytes {
						return Report{}, ErrUnavailable
					}
					secrets = append(secrets, value)
				}
			}
			harness, thread := Detect(args)
			if harness != "other" && p.Harness == "other" {
				p.Harness, p.ThreadID = harness, thread
				harnessTree[pid] = true
			} else if harnessTree[observed.Parent] && (harness == "other" || harness == p.Harness) {
				harnessTree[pid] = true
			}
			if d.ThreadID != nil && p.ThreadID == "" && harnessTree[pid] && (p.Harness == "claude" || p.Harness == "codex") {
				fdProcesses++
				if fdProcesses > maxThreadProcesses {
					fdFailed = true
					r.Truncated = true
				} else {
					id, err := d.ThreadID(ctx, observed, p.Harness)
					if err != nil || (id != "" && !uuid.MatchString(id)) {
						fdFailed = true
					} else if id != "" {
						fdThreads[strings.ToLower(id)] = true
					}
				}
			}
			queue = append(queue, children[pid]...)
		}
		if p.ThreadID == "" {
			if fdFailed || len(fdThreads) > 1 {
				r.Unavailable = append(r.Unavailable, "thread_fds")
			} else {
				for id := range fdThreads {
					p.ThreadID = id
				}
			}
		}
		// A declared holder can be an ancestor of the pane's shell.
		for pid, depth := raw.PID, 0; pid > 0 && depth < MaxProcesses; depth++ {
			parent, ok := byPID[pid]
			if !ok || blocked[pid] || parent.TTY != raw.TTY {
				break
			}
			if name := declarations[pid]; name != "" {
				p.DeclaredSession = name
				break
			}
			if parent.Parent == pid {
				break
			}
			pid = parent.Parent
		}
		if d.Git != nil && filepath.IsAbs(raw.Cwd) {
			work, cached := gitCache[raw.Cwd]
			if !cached {
				work, err = d.Git(ctx, raw.Cwd)
				if err != nil {
					r.Unavailable = append(r.Unavailable, "git")
				}
				gitCache[raw.Cwd] = work
			}
			if work != nil {
				copy := *work
				copy.Remotes = append([]string{}, work.Remotes...)
				p.Worktree = &copy
			}
		}
		r.Panes = append(r.Panes, p)
	}
	// Redact after all argv are observed, so a secret found in a later pane
	// also scrubs an earlier pane's cwd/branch/name. Never serialize the cache.
	for i := range r.Panes {
		p := &r.Panes[i]
		p.Session = clean(p.Session, secrets)
		p.Window = clean(p.Window, secrets)
		p.Target = clean(p.Target, secrets)
		p.Command = clean(p.Command, secrets)
		p.Cwd = clean(p.Cwd, secrets)
		p.DeclaredSession = clean(p.DeclaredSession, secrets)
		p.ThreadID = clean(p.ThreadID, secrets)
		if p.Worktree != nil {
			w := p.Worktree
			w.Path = clean(w.Path, secrets)
			w.Branch = clean(w.Branch, secrets)
			w.Head = clean(w.Head, secrets)
			for j := range w.Remotes {
				w.Remotes[j] = clean(w.Remotes[j], secrets)
			}
		}
	}
	sort.Strings(r.Unavailable)
	r.Unavailable = unique(r.Unavailable)
	return r, nil
}

func unique(values []string) []string {
	out := values[:0]
	for _, s := range values {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// Encode is shared by CLI and job publication. Remove complete rows rather
// than slicing JSON; every successfully published document stays <=64 KiB.
func Encode(r Report) ([]byte, error) {
	// Optional capacity observations fail to unknown, never reject the report.
	for _, value := range []**float64{&r.Capacity.Load1, &r.Capacity.Load5} {
		if *value != nil && (math.IsNaN(**value) || math.IsInf(**value, 0) || **value < 0) {
			*value = nil
			r.Unavailable = append(append([]string{}, r.Unavailable...), "capacity")
		}
	}
	if r.Capacity.NCPU < 0 {
		r.Capacity.NCPU = 0
	}
	components := []string{}
	for _, component := range r.Unavailable {
		switch component {
		case "processes", "declarations", "capacity", "pane_process", "argv", "git", "terminal_scope", "thread_fds":
			components = append(components, component)
		}
	}
	sort.Strings(components)
	r.Unavailable = unique(components)
	// Defense in depth at both publication boundaries, including a future
	// collector implementation. Do not mutate the caller's cached snapshot.
	r.Panes = append([]Pane{}, r.Panes...)
	if len(r.Panes) > MaxPanes {
		r.Panes = r.Panes[:MaxPanes]
		r.Truncated = true
	}
	for i := range r.Panes {
		p := &r.Panes[i]
		p.Session = clean(p.Session, nil)
		p.Window = clean(p.Window, nil)
		p.Target = clean(p.Target, nil)
		p.Command = clean(p.Command, nil)
		p.Cwd = clean(p.Cwd, nil)
		p.DeclaredSession = clean(p.DeclaredSession, nil)
		if p.Harness != "codex" && p.Harness != "claude" && p.Harness != "qwen" && p.Harness != "ace" {
			p.Harness = "other"
		}
		if !uuid.MatchString(p.ThreadID) {
			p.ThreadID = ""
		}
		if p.Worktree != nil {
			w := *p.Worktree
			w.Path = clean(w.Path, nil)
			w.Branch = clean(w.Branch, nil)
			if !gitHead.MatchString(w.Head) {
				w.Head = ""
				r.Unavailable = append(r.Unavailable, "git")
			}
			remotes := w.Remotes
			if len(remotes) > maxRemotes {
				remotes = remotes[:maxRemotes]
				r.Truncated = true
			}
			w.Remotes = []string{}
			for _, name := range remotes {
				if len(name) > maxRemoteBytes {
					r.Truncated = true
					continue
				}
				w.Remotes = append(w.Remotes, clean(name, nil))
			}
			p.Worktree = &w
		}
	}
	sort.Strings(r.Unavailable)
	r.Unavailable = unique(r.Unavailable)
	for {
		raw, err := json.Marshal(r)
		if err != nil {
			return nil, ErrUnavailable
		}
		if len(raw)+1 <= MaxBytes {
			return raw, nil
		}
		if len(r.Panes) == 0 {
			return nil, ErrUnavailable
		}
		// JSON escaping can expand a single row up to sixfold. Shed the
		// largest encoded row first, not every healthy row following it.
		largest, largestBytes := 0, 0
		for i, pane := range r.Panes {
			encoded, err := json.Marshal(pane)
			if err != nil {
				return nil, ErrUnavailable
			}
			if len(encoded) > largestBytes {
				largest, largestBytes = i, len(encoded)
			}
		}
		r.Panes = append(r.Panes[:largest], r.Panes[largest+1:]...)
		r.Truncated = true
	}
}
