// Package sessionsearch implements bounded, owner-local search without an index.
package sessionsearch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

const (
	maxFiles      = 4096
	maxSessions   = 4096
	maxFileBytes  = 16 << 20
	maxTotalBytes = 128 << 20
	maxLineBytes  = 1 << 20
	withheld      = "withheld: possible secret"
)

var ErrUnavailable = errors.New("local session search unavailable")
var ErrOptions = errors.New("invalid session search options")
var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var rollout = regexp.MustCompile(`^rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-([0-9a-fA-F-]{36})\.jsonl$`)
var paneTarget = regexp.MustCompile(`^%[0-9]+$`)
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

type Options struct {
	Query        string
	Harness      string
	Since, Until time.Time
}
type Snippet struct {
	Kind string    `json:"kind"`
	Text string    `json:"text"`
	Time time.Time `json:"time"`
}
type Hit struct {
	ID         string    `json:"session_id"`
	Harness    string    `json:"harness"`
	Title      string    `json:"title"`
	Cwd        string    `json:"cwd,omitempty"`
	State      string    `json:"state"`
	LastActive time.Time `json:"last_active"`
	Snippets   []Snippet `json:"snippets"`
	Location   string    `json:"location,omitempty"`
	Open       string    `json:"open,omitempty"`
}
type Report struct {
	Hits        []Hit     `json:"hits"`
	Partial     bool      `json:"partial"`
	Unavailable []string  `json:"unavailable,omitempty"`
	Oldest      time.Time `json:"oldest"`
}
type group struct {
	Hit
	present, matched bool
}
type search struct {
	ctx       context.Context
	opts      Options
	words     []string
	root      *os.Root
	uid       int
	groups    map[string]*group
	report    Report
	files     int
	remaining int64
}

// Search reads only fixed harness roots below the supplied owner home. Callers
// must not expose home/uid as remote request parameters. Native supplies them
// from the OS account, not environment variables. Inventory is observation only.
func Search(ctx context.Context, home string, uid int, opts Options, panes []laneinventory.Pane) (Report, error) {
	if strings.TrimSpace(opts.Query) == "" || len(opts.Query) > 1024 || len(strings.Fields(opts.Query)) > 32 ||
		(opts.Harness != "" && opts.Harness != "claude" && opts.Harness != "codex") ||
		(!opts.Until.IsZero() && !opts.Since.IsZero() && opts.Until.Before(opts.Since)) {
		return Report{}, ErrOptions
	}
	if !filepath.IsAbs(home) || !ownerSupported(uid) {
		return Report{}, ErrUnavailable
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return Report{}, ErrUnavailable
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !owned(info, uid) {
		return Report{}, ErrUnavailable
	}
	s := &search{ctx: ctx, opts: opts, words: strings.Fields(strings.ToLower(opts.Query)), root: root, uid: uid,
		groups: make(map[string]*group), remaining: maxTotalBytes, report: Report{Hits: []Hit{}}}
	for _, harness := range []string{"claude", "codex"} {
		if opts.Harness != "" && opts.Harness != harness {
			continue
		}
		s.scan(filepath.Join("."+harness, "history.jsonl"), harness, "", true)
		base := filepath.Join(".claude", "projects")
		depth := 2
		if harness == "codex" {
			base = filepath.Join(".codex", "sessions")
			depth = 4
		}
		s.walk(base, harness, depth)
	}
	for _, g := range s.groups {
		if !g.matched && !g.LastActive.IsZero() && g.Title != "" && g.Title != withheld {
			g.matched = true
			for _, word := range s.words {
				if !strings.Contains(strings.ToLower(g.Title), word) {
					g.matched = false
					break
				}
			}
		}
		if !g.matched {
			continue
		}
		g.State = "pruned"
		if s.report.Partial {
			g.State = "unknown"
		}
		if g.present {
			g.State = "ended"
			g.Open = "claude -r " + g.ID
			if g.Harness == "codex" {
				g.Open = "codex resume " + g.ID
			}
		}
		if g.Title == "" {
			g.Title = g.Harness + " " + g.ID[:8]
		}
		s.report.Hits = append(s.report.Hits, g.Hit)
	}
	sort.Slice(s.report.Hits, func(i, j int) bool {
		a, b := s.report.Hits[i], s.report.Hits[j]
		if a.LastActive.Equal(b.LastActive) {
			return a.Harness+a.ID < b.Harness+b.ID
		}
		return a.LastActive.After(b.LastActive)
	})
	if len(s.report.Hits) > 20 {
		s.report.Hits = s.report.Hits[:20]
		s.report.Partial = true
	}
	for i := range s.report.Hits {
		if ctx.Err() == nil {
			s.hookName(&s.report.Hits[i])
		}
	}
	if ctx.Err() != nil {
		s.report.Partial = true
	}
	Locate(&s.report, panes)
	return s.report, nil
}

// Locate adds only validated, unambiguous observation data. It never attaches.
func Locate(report *Report, panes []laneinventory.Pane) {
	for i := range report.Hits {
		g := &report.Hits[i]
		var matches []laneinventory.Pane
		for _, p := range panes {
			if strings.EqualFold(p.ThreadID, g.ID) && p.Harness == g.Harness {
				matches = append(matches, p)
			}
		}
		if len(matches) != 1 {
			continue
		}
		p := matches[0]
		if !paneTarget.MatchString(p.Target) {
			continue
		}
		g.State = "running"
		g.Location = p.Target
		g.Open = "tmux switch-client -t " + p.Target
		if validName(p.DeclaredSession) && safe(p.DeclaredSession) != withheld {
			g.Title = safe(p.DeclaredSession)
			g.Open = "citadel attach " + p.DeclaredSession
		}
	}
}

func safe(text string) string {
	// Inspect the entire value before clipping, so slicing cannot hide a token.
	if len(trust.CheckSecrets("", text)) != 0 {
		return withheld
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	r := []rune(strings.Join(strings.Fields(text), " "))
	if len(r) > 240 {
		r = r[:240]
	}
	return string(r)
}
func validName(s string) bool { return namePattern.MatchString(s) }
func (s *search) get(harness, id string) *group {
	if !uuid.MatchString(id) {
		return nil
	}
	id = strings.ToLower(id)
	key := harness + ":" + id
	if g := s.groups[key]; g != nil {
		return g
	}
	if len(s.groups) >= maxSessions {
		s.report.Partial = true
		return nil
	}
	g := &group{Hit: Hit{ID: id, Harness: harness, Snippets: []Snippet{}}}
	s.groups[key] = g
	return g
}
func (s *search) record(g *group, kind, text string, at time.Time) {
	if g == nil || at.IsZero() {
		return
	}
	if s.report.Oldest.IsZero() || at.Before(s.report.Oldest) {
		s.report.Oldest = at
	}
	if !s.opts.Since.IsZero() && at.Before(s.opts.Since) || !s.opts.Until.IsZero() && at.After(s.opts.Until) {
		return
	}
	if at.After(g.LastActive) {
		g.LastActive = at
	}
	if g.Title == "" && kind == "prompt" {
		g.Title = safe(text)
	}
	lower := strings.ToLower(text)
	for _, word := range s.words {
		if !strings.Contains(lower, word) {
			return
		}
	}
	g.matched = true
	// Centre the snippet on the first search word, then apply the shared guard.
	output := safe(text)
	if output != withheld {
		runes := []rune(text)
		index := strings.Index(lower, s.words[0])
		start := len([]rune(text[:min(index, len(text))])) - 80
		if start < 0 {
			start = 0
		}
		output = safe(string(runes[start:min(start+240, len(runes))]))
	}
	g.Snippets = append(g.Snippets, Snippet{kind, output, at})
	sort.SliceStable(g.Snippets, func(i, j int) bool { return g.Snippets[i].Time.After(g.Snippets[j].Time) })
	if len(g.Snippets) > 3 {
		g.Snippets = g.Snippets[:3]
	}
}
func (s *search) unavailable(component string) {
	s.report.Partial = true
	for _, v := range s.report.Unavailable {
		if v == component {
			return
		}
	}
	s.report.Unavailable = append(s.report.Unavailable, component)
}
func (s *search) walk(path, harness string, depth int) {
	if s.ctx.Err() != nil || s.files >= maxFiles || s.remaining <= 0 {
		s.report.Partial = true
		return
	}
	f, err := openOwned(s.root, path, s.uid, true)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		s.unavailable(harness)
		return
	}
	defer f.Close()
	entries, err := f.ReadDir(maxFiles - s.files + 1)
	if err != nil && err != io.EOF {
		s.unavailable(harness)
		return
	}
	for _, entry := range entries {
		s.files++
		if s.files > maxFiles || s.remaining <= 0 || s.ctx.Err() != nil {
			s.report.Partial = true
			return
		}
		if entry.Type()&os.ModeSymlink != 0 {
			s.unavailable(harness)
			continue
		}
		child := filepath.Join(path, entry.Name())
		if depth > 1 && entry.IsDir() {
			s.walk(child, harness, depth-1)
			continue
		}
		if depth != 1 || entry.IsDir() {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		if harness == "codex" {
			match := rollout.FindStringSubmatch(entry.Name())
			if len(match) != 2 {
				continue
			}
			id = match[1]
		}
		if uuid.MatchString(id) && strings.HasSuffix(entry.Name(), ".jsonl") {
			s.scan(child, harness, id, false)
		}
	}
}
func (s *search) scan(path, harness, id string, history bool) {
	if s.ctx.Err() != nil || s.remaining <= 0 {
		s.report.Partial = true
		return
	}
	f, err := openOwned(s.root, path, s.uid, false)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		s.unavailable(harness)
		return
	}
	defer f.Close()
	if !history {
		if g := s.get(harness, id); g != nil {
			g.present = true
		}
	}
	limit := min(int64(maxFileBytes), s.remaining)
	reader := &io.LimitedReader{R: f, N: limit}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 65536), maxLineBytes)
	for scanner.Scan() {
		if s.ctx.Err() != nil {
			s.report.Partial = true
			break
		}
		s.line(scanner.Bytes(), harness, id, history)
	}
	s.remaining -= limit - reader.N
	if scanner.Err() != nil || reader.N == 0 {
		s.unavailable(harness)
	}
}

type entry struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"sessionId"`
	CodexID     string          `json:"session_id"`
	Display     string          `json:"display"`
	Project     string          `json:"project"`
	Cwd         string          `json:"cwd"`
	Text        string          `json:"text"`
	Timestamp   json.RawMessage `json:"timestamp"`
	TS          int64           `json:"ts"`
	Title       string          `json:"customTitle"`
	AgentName   string          `json:"agentName"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Payload struct {
		ID      string          `json:"id"`
		Cwd     string          `json:"cwd"`
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"payload"`
}

func (s *search) line(raw []byte, harness, id string, history bool) {
	var e entry
	if json.Unmarshal(raw, &e) != nil {
		s.unavailable(harness)
		return
	}
	var at time.Time
	var stamp string
	if json.Unmarshal(e.Timestamp, &stamp) == nil {
		at, _ = time.Parse(time.RFC3339Nano, stamp)
	} else {
		var ms int64
		if json.Unmarshal(e.Timestamp, &ms) == nil && ms > 0 {
			at = time.UnixMilli(ms).UTC()
		}
	}
	if history {
		text := e.Display
		id = e.SessionID
		if harness == "codex" {
			id = e.CodexID
			text = e.Text
			if e.TS > 0 {
				at = time.Unix(e.TS, 0).UTC()
			}
		}
		g := s.get(harness, id)
		if g != nil && filepath.IsAbs(e.Project) {
			g.Cwd = safe(e.Project)
		}
		s.record(g, "prompt", text, at)
		return
	}
	g := s.get(harness, id)
	if g == nil || e.IsMeta || e.IsSidechain {
		return
	}
	if filepath.IsAbs(e.Cwd) {
		g.Cwd = safe(e.Cwd)
	}
	if harness == "codex" && e.Type == "session_meta" && strings.EqualFold(e.Payload.ID, g.ID) && filepath.IsAbs(e.Payload.Cwd) {
		g.Cwd = safe(e.Payload.Cwd)
	}
	if harness == "claude" && (e.Type == "custom-title" || e.Type == "agent-name") {
		title := e.Title
		if title == "" {
			title = e.AgentName
		}
		g.Title = safe(title)
		return
	}
	role, content := e.Message.Role, e.Message.Content
	if harness == "claude" && e.Type != "assistant" {
		return
	}
	if harness == "codex" {
		if e.Type != "response_item" || e.Payload.Type != "message" {
			return
		}
		role, content = e.Payload.Role, e.Payload.Content
	}
	if role != "assistant" {
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for _, rawBlock := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(rawBlock, &block) != nil {
			s.unavailable(harness)
			continue
		}
		if block.Type == "text" && harness == "claude" || block.Type == "output_text" && harness == "codex" {
			s.record(g, "reply", block.Text, at)
		}
	}
}
