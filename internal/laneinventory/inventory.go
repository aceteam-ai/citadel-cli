// Package laneinventory projects bounded, read-only lane metadata. Native
// observation is behind explicit seams; raw argv and git status never serialize.
package laneinventory

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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
	Timeout        = 15 * time.Second
)

var ErrUnavailable = errors.New("lane inventory observation unavailable")

type RawPane struct {
	Session, Window, Target, Command, Cwd string
	PID                                   int32
}

type Process struct {
	PID, Parent, UID int32
	StartedAt        int64 // Unix milliseconds, matching the process observer.
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
	Argv         func(context.Context, int32) ([]string, error)
	Declarations func(context.Context) (map[int32]string, error)
	Git          func(context.Context, string) (*Worktree, error)
	Capacity     func(context.Context) (Capacity, error)
}

type Collector struct{ Deps Deps }

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

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
	if len(panes) > MaxPanes {
		panes = panes[:MaxPanes]
		r.Truncated = true
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
	for _, raw := range panes {
		if ctx.Err() != nil {
			return Report{}, ErrUnavailable
		}
		p := Pane{Session: raw.Session, Window: raw.Window, Target: raw.Target, Command: filepath.Base(raw.Command), Cwd: raw.Cwd, Harness: "other"}
		root, own := byPID[raw.PID]
		if !own {
			r.Unavailable = append(r.Unavailable, "pane_process")
			continue
		}
		if root.StartedAt > 0 {
			value := root.StartedAt
			p.StartedAt = &value
		}
		queue := []int32{raw.PID}
		seen := map[int32]bool{}
		for len(queue) > 0 {
			if ctx.Err() != nil {
				return Report{}, ErrUnavailable
			}
			pid := queue[0]
			queue = queue[1:]
			if seen[pid] {
				continue
			}
			seen[pid] = true
			if name := declarations[pid]; name != "" {
				p.DeclaredSession = name
			}
			args, cached := argvCache[pid]
			if !cached {
				args, err = d.Argv(ctx, pid)
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
			}
			queue = append(queue, children[pid]...)
		}
		// A declared holder can be an ancestor of the pane's shell.
		for pid, depth := raw.PID, 0; pid > 0 && depth < MaxProcesses; depth++ {
			parent, ok := byPID[pid]
			if !ok {
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
		case "processes", "declarations", "capacity", "pane_process", "argv", "git":
			components = append(components, component)
		}
	}
	sort.Strings(components)
	r.Unavailable = unique(components)
	// Defense in depth at both publication boundaries, including a future
	// collector implementation. Do not mutate the caller's cached snapshot.
	r.Panes = append([]Pane{}, r.Panes...)
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
			w.Head = clean(w.Head, nil)
			w.Remotes = append([]string{}, w.Remotes...)
			for j := range w.Remotes {
				w.Remotes[j] = clean(w.Remotes[j], nil)
			}
			p.Worktree = &w
		}
	}
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
		r.Panes = r.Panes[:len(r.Panes)-1]
		r.Truncated = true
	}
}
