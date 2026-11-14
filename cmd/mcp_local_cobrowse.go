// cmd/mcp_local_cobrowse.go
//
// The local_cobrowse_* MCP tools (aceteam#13503 S3, design section 2.3). They
// mirror the platform's cobrowse_* tools minus node_id, and there is no pin
// argument anywhere: in local mode a secret is only ever entered by the human
// in the local viewer, never through MCP. internal/localcobrowse owns consent,
// limits, the address filter and the single-owner rule; this file only adapts
// JSON arguments and results.
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// localCobrowseRuntime owns one process's co-browse service and, once the first
// co-browse call acquires it, the per-user owner lock.
type localCobrowseRuntime struct {
	svc *localcobrowse.Service

	mu       sync.Mutex
	owner    *localcobrowse.Owner
	guard    *localcobrowse.EgressGuard
	viewer   *localviewer.Server
	audit    *localaudit.Log
	auditErr error
	auditDir string
	stop     chan struct{}

	// closeOnce runs the teardown once; closed is closed when it has
	// finished, so every caller of close waits for the profiles to be sealed.
	closeOnce sync.Once
	closed    chan struct{}

	// viewerFactory builds the loopback viewer on first need; nil disables it.
	viewerFactory func() (*localviewer.Server, error)
}

// localCobrowseOptions customizes the runtime; zero values are production.
type localCobrowseOptions struct {
	// ownerDir resolves where the owner marker, audit log and viewer token
	// live (localStateDir in production). Resolved only when co-browse is
	// first used.
	ownerDir func() string
	browser  localcobrowse.Browser
	policy   localcobrowse.URLPolicy
	// viewerPorts, onAttention and onEvent are wired by later slices (viewer,
	// audit).
	viewerPorts func() []int
	onAttention func(id, reason string)
	onEvent     func(localcobrowse.Event)
	// viewer enables the loopback viewer (production). openURL overrides the
	// default-browser opener (tests).
	viewer  bool
	openURL func(string) error
	// auditRetention overrides the audit retention (default 90 days, or
	// CITADEL_LOCAL_AUDIT_RETENTION_DAYS).
	auditRetention time.Duration
}

// localAuditRetention reads CITADEL_LOCAL_AUDIT_RETENTION_DAYS (1 to 3650).
func localAuditRetention() time.Duration {
	if v := strings.TrimSpace(os.Getenv("CITADEL_LOCAL_AUDIT_RETENTION_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 3650 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return localaudit.DefaultRetention
}

// auditRecordFor maps a service event to an audit record. Typed text never
// reaches here: the event carries only its length.
func auditRecordFor(ev localcobrowse.Event) localaudit.Record {
	actor := "agent"
	if ev.Class == "H" {
		actor = "user"
	}
	rules := ""
	if ev.Fingerprint != "" {
		rules = localcobrowse.RuleVersion
	}
	return localaudit.Record{
		Domain:          ev.Domain,
		Fingerprint:     ev.Fingerprint,
		Label:           ev.Label,
		Rules:           rules,
		SessionID:       ev.SessionID,
		Actor:           actor,
		Agent:           ev.Agent,
		IntendedAction:  ev.Action,
		Class:           ev.Class,
		Goal:            ev.Goal,
		URL:             ev.URL,
		Selector:        ev.Selector,
		TextLen:         ev.TextLen,
		ResultingAction: ev.Outcome,
		PendingSeq:      ev.PendingSeq,
		Error:           ev.Error,
	}
}

// recordEvent appends ev to the audit log. Only the owner writes (one writer
// per chain). A failed write turns co-browse off: Precheck refuses every later
// call, so nothing runs unaudited.
func (rt *localCobrowseRuntime) recordEvent(ev localcobrowse.Event) {
	rt.mu.Lock()
	log := rt.audit
	rt.mu.Unlock()
	if log == nil {
		return
	}
	if _, err := log.Append(auditRecordFor(ev)); err != nil {
		rt.mu.Lock()
		if rt.auditErr == nil {
			rt.auditErr = fmt.Errorf("the local audit log in %s cannot be written (%v); co-browse is off until Citadel restarts", log.Dir(), err)
		}
		rt.mu.Unlock()
	}
}

// recordPending appends the pending record for a write about to run. If it
// cannot be written the write is refused and co-browse turns off.
func (rt *localCobrowseRuntime) recordPending(ev localcobrowse.Event) (int64, error) {
	rt.mu.Lock()
	log := rt.audit
	rt.mu.Unlock()
	if log == nil {
		return 0, nil
	}
	rec, err := log.Append(auditRecordFor(ev))
	if err != nil {
		rt.mu.Lock()
		if rt.auditErr == nil {
			rt.auditErr = fmt.Errorf("the local audit log in %s cannot be written (%v); co-browse is off until Citadel restarts. You can still stop or take over sessions in the viewer", log.Dir(), err)
		}
		e := rt.auditErr
		rt.mu.Unlock()
		return 0, e
	}
	return rec.Seq, nil
}

func (rt *localCobrowseRuntime) auditPrecheck() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.auditErr
}

func newLocalCobrowseRuntime(opts localCobrowseOptions) *localCobrowseRuntime {
	rt := &localCobrowseRuntime{stop: make(chan struct{}), closed: make(chan struct{})}
	browser := opts.browser
	var manager *platform.CobrowseSessionManager
	if browser == nil {
		manager = platform.NewCobrowseSessionManager(platform.DefaultCobrowseSessionsBaseDir(), localcobrowse.DefaultMaxSessions)
		browser = manager
	} else if m, ok := browser.(*platform.CobrowseSessionManager); ok {
		manager = m
	}
	viewerPorts, onAttention, viewerNote := opts.viewerPorts, opts.onAttention, ""
	if opts.viewer {
		openURL := opts.openURL
		if openURL == nil {
			openURL = platform.OpenURL
		}
		rt.viewerFactory = func() (*localviewer.Server, error) {
			cfg := localviewer.Config{Service: rt.svc, OpenURL: openURL, MaxSessions: localcobrowse.DefaultMaxSessions}
			if opts.ownerDir != nil {
				dir := filepath.Join(opts.ownerDir(), localaudit.DirName)
				cfg.Activity = func(session string, limit int) ([]localaudit.Record, error) {
					return localaudit.Read(dir, localaudit.Filter{SessionID: session, Limit: limit})
				}
			}
			if manager != nil {
				cfg.Stream = cobrowsestream.NewManagerSessionFrom(manager)
			}
			if opts.ownerDir != nil {
				cfg.TokenDir = opts.ownerDir()
			}
			v := localviewer.New(cfg)
			if err := v.Start(); err != nil {
				return nil, err
			}
			return v, nil
		}
		viewerPorts = rt.viewerPorts
		onAttention = rt.attention
		viewerNote = "The Citadel viewer opens in the user's default browser, where they watch, approve and take control. If it is closed, they can run `citadel local viewer`."
	}
	var profiles localcobrowse.ProfileStore
	if opts.ownerDir != nil {
		profiles = localProfileStore{dir: opts.ownerDir}
	}
	rt.svc = localcobrowse.New(localcobrowse.Config{
		Browser:     browser,
		Profiles:    profiles,
		URLPolicy:   opts.policy,
		ViewerPorts: viewerPorts,
		OnAttention: onAttention,
		OnEvent: func(ev localcobrowse.Event) {
			rt.recordEvent(ev)
			if opts.onEvent != nil {
				opts.onEvent(ev)
			}
		},
		Precheck:     rt.auditPrecheck,
		BeforeAction: rt.recordPending,
		ViewerNote:   viewerNote,
		AcquireOwner: func() error {
			if opts.ownerDir == nil {
				return nil
			}
			dir := opts.ownerDir()
			o, err := localcobrowse.AcquireOwner(dir)
			if err != nil {
				return err
			}
			retention := opts.auditRetention
			if retention <= 0 {
				retention = localAuditRetention()
			}
			auditDir := filepath.Join(dir, localaudit.DirName)
			log, err := localaudit.Open(auditDir, retention, nil)
			if err != nil {
				o.Release()
				return fmt.Errorf("co-browse needs its local audit log, which cannot be opened in %s: %w", auditDir, err)
			}
			rt.mu.Lock()
			rt.audit, rt.auditDir = log, auditDir
			rt.mu.Unlock()
			if manager != nil {
				// Every connection of the real browser goes through the
				// egress guard. If it cannot start, co-browse refuses: the
				// URL filter alone does not see redirects or subresources.
				g, gerr := localcobrowse.StartEgressGuard(nil, nil)
				if gerr != nil {
					// Drop the log before the lock: once released, another
					// process may own (and write) this chain.
					rt.mu.Lock()
					rt.audit = nil
					rt.mu.Unlock()
					o.Release()
					return gerr
				}
				manager.SetLaunchOptions(platform.LaunchOptions{ProxyServer: g.ProxyURL(), Quiet: true})
				rt.mu.Lock()
				rt.guard = g
				rt.mu.Unlock()
			}
			if agent := rt.svc.Agent(); agent != "" {
				o.SetClient(agent)
			}
			rt.mu.Lock()
			rt.owner = o
			rt.mu.Unlock()
			return nil
		},
	})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-rt.stop:
				return
			case <-t.C:
				rt.svc.ReapIdle()
			}
		}
	}()
	return rt
}

// ensureViewer starts the loopback viewer once.
func (rt *localCobrowseRuntime) ensureViewer() *localviewer.Server {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.viewer != nil || rt.viewerFactory == nil {
		return rt.viewer
	}
	select {
	case <-rt.stop:
		return nil
	default:
	}
	v, err := rt.viewerFactory()
	if err != nil {
		Debug("local viewer failed to start: %v", err)
		return nil
	}
	rt.viewer = v
	return v
}

// attention is the service's OnAttention hook: start the viewer if needed and
// show it to the user.
func (rt *localCobrowseRuntime) attention(id, reason string) {
	if v := rt.ensureViewer(); v != nil {
		v.Attention(id, reason)
	}
}

// viewerPorts reports the viewer's port so the address filter refuses it.
func (rt *localCobrowseRuntime) viewerPorts() []int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.viewer == nil {
		return nil
	}
	return []int{rt.viewer.Port()}
}

// openViewer opens a fresh viewer link (starting the viewer if needed).
func (rt *localCobrowseRuntime) openViewer() error {
	v := rt.ensureViewer()
	if v == nil {
		return fmt.Errorf("the local viewer is not available")
	}
	return v.Open()
}

// setAgent records the MCP client name from initialize.
func (rt *localCobrowseRuntime) setAgent(name string) {
	rt.svc.SetAgent(name)
	rt.mu.Lock()
	o := rt.owner
	rt.mu.Unlock()
	if o != nil {
		o.SetClient(name)
	}
}

// close stops every session and releases the owner lock. It is idempotent and
// every caller waits for the first teardown to finish: a saved-logins session
// is re-sealed during that teardown, so returning early (for example to a
// signal handler about to exit) would leave its plaintext working copy on
// disk and lose its new logins.
func (rt *localCobrowseRuntime) close() {
	rt.closeOnce.Do(func() {
		defer close(rt.closed)
		rt.teardown()
	})
	<-rt.closed
}

func (rt *localCobrowseRuntime) teardown() {
	close(rt.stop)
	_ = rt.svc.Close()
	rt.mu.Lock()
	o, g, v := rt.owner, rt.guard, rt.viewer
	rt.owner, rt.guard, rt.viewer = nil, nil, nil
	rt.mu.Unlock()
	if v != nil {
		v.Close()
	}
	if g != nil {
		g.Close()
	}
	if o != nil {
		o.Release()
	}
}

func jsonText(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// argError marks a call refused for its arguments before it reached the
// service, so the tool wrapper can still audit it.
type argError struct{ error }

func (e argError) Unwrap() error { return e.error }

func decodeArgs(args json.RawMessage, into any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, into); err != nil {
		return argError{fmt.Errorf("invalid arguments: %w", err)}
	}
	return nil
}

func requireSession(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", argError{fmt.Errorf("'session_id' is required (from local_cobrowse_start)")}
	}
	return id, nil
}

// recordArgRefusal audits a call refused for its arguments.
func recordArgRefusal(s *localcobrowse.Service, tool string, args json.RawMessage, err error) {
	var ae argError
	if !errors.As(err, &ae) {
		return
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(args, &in)
	s.RecordRefusal(strings.TrimSpace(in.SessionID), strings.TrimPrefix(tool, "local_cobrowse_"), "", err)
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func numProp(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

const (
	descSession = "A session id from local_cobrowse_start."
	descGoal    = "Optional short description of what you are trying to do, recorded in the local audit log."
)

// newLocalCobrowseTools builds the local_cobrowse_* tools. A nil runtime yields
// tools that explain co-browse is unavailable in this process.
func newLocalCobrowseTools(rt *localCobrowseRuntime) []localMCPTool {
	svc := func() (*localcobrowse.Service, error) {
		if rt == nil || rt.svc == nil {
			return nil, fmt.Errorf("local co-browse is not available in this Citadel process")
		}
		return rt.svc, nil
	}
	writeTool := func(name, desc string, props map[string]any, required []string,
		do func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error)) localMCPTool {
		return localMCPTool{
			Name: name, Description: desc, InputSchema: objSchema(props, required...), Async: true,
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				s, err := svc()
				if err != nil {
					return "", err
				}
				v, err := do(ctx, s, args)
				if err != nil {
					recordArgRefusal(s, name, args, err)
					return "", err
				}
				return jsonText(v)
			},
		}
	}

	writeNote := " A WRITE: needs the user's approval for the site (its registrable domain) in the Citadel viewer. Without it the call returns approval_required; then call local_cobrowse_wait with until=approved. Anything that sends, posts, submits, pays or deletes (a Send or Post button, Enter in a message or form field, an unlabeled or unidentifiable target) needs the user's approval every time, whatever was approved: the call returns commit_approval_required; call local_cobrowse_wait with until=commit, then repeat the same call. Refused while the user has control."
	readNote := " A READ: no approval needed. Refused while the user has control, so you never see what they type."

	type clickArgs struct {
		SessionID string   `json:"session_id"`
		Selector  string   `json:"selector"`
		X         *float64 `json:"x"`
		Y         *float64 `json:"y"`
		Goal      string   `json:"goal"`
	}
	type scrollArgs struct {
		clickArgs
		DeltaX float64 `json:"delta_x"`
		DeltaY float64 `json:"delta_y"`
	}

	return []localMCPTool{
		writeTool("local_cobrowse_start",
			"Start a browser session on THIS computer (Citadel local mode). The browser runs headless and the user watches and takes over in the local Citadel viewer. Opening the start URL needs no approval; later navigate/click/type do. Private, local and non-web addresses are refused. At most 2 sessions at once; a session ends on local_cobrowse_stop, when this MCP server exits, or after 8 hours without activity.",
			map[string]any{
				"url":     strProp("Optional http(s) URL to open on launch."),
				"goal":    strProp(descGoal),
				"profile": strProp("Optional saved-logins profile name (1 to 64 lowercase letters, digits or dashes, e.g. \"default\"). Omit for a throwaway session that starts logged out. With a profile the session starts locked: the user unlocks it with their passphrase in the Citadel viewer (you never see it), so call local_cobrowse_wait with until=unlocked. Logins are encrypted on this computer and kept for next time."),
			}, nil,
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct{ URL, Goal, Profile string }
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return s.Start(ctx, in.URL, in.Goal, in.Profile)
			}),
		writeTool("local_cobrowse_status",
			"Show local co-browse sessions: state, who is driving, URL, approval state and expiry, a pending handoff, and the lifetime policy. Omit session_id to list all.",
			map[string]any{"session_id": strProp("Optional session id.")}, nil,
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID string `json:"session_id"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				views, err := s.Status(ctx, strings.TrimSpace(in.SessionID))
				if err != nil {
					return nil, err
				}
				return map[string]any{"sessions": views, "agent": s.Agent()}, nil
			}),
		writeTool("local_cobrowse_navigate", "Open a URL in a local session."+writeNote,
			map[string]any{"session_id": strProp(descSession), "url": strProp("The http(s) URL to open."), "goal": strProp(descGoal)},
			[]string{"session_id", "url"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID string `json:"session_id"`
					URL, Goal string
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				return s.Navigate(ctx, id, in.URL, in.Goal)
			}),
		writeTool("local_cobrowse_click", "Click an element by CSS selector, or a viewport point by x and y. Returns dispatched, settled and navigated; a slow page after the click is not a failure, so do not repeat a dispatched click."+writeNote,
			map[string]any{"session_id": strProp(descSession), "selector": strProp("CSS selector; wins over x/y."),
				"x": numProp("Viewport X in CSS pixels (with y)."), "y": numProp("Viewport Y in CSS pixels (with x)."), "goal": strProp(descGoal)},
			[]string{"session_id"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in clickArgs
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				if in.Selector == "" && (in.X == nil || in.Y == nil) {
					return nil, argError{fmt.Errorf("click requires 'selector' or both 'x' and 'y'")}
				}
				return s.Click(ctx, id, in.Selector, in.X, in.Y, in.Goal)
			}),
		writeTool("local_cobrowse_type", "Type text into the focused field (click it first). Only the text length is recorded. Never use this for passwords: hand off to the user instead."+writeNote,
			map[string]any{"session_id": strProp(descSession), "text": strProp("Text to insert."), "goal": strProp(descGoal)},
			[]string{"session_id", "text"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID  string `json:"session_id"`
					Text, Goal string
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				return s.Type(ctx, id, in.Text, in.Goal)
			}),
		writeTool("local_cobrowse_select", "Choose an option in a native select element by value or label."+writeNote,
			map[string]any{"session_id": strProp(descSession), "selector": strProp("CSS selector of the select element."),
				"value": strProp("Option value."), "label": strProp("Option label, used when value is empty."), "goal": strProp(descGoal)},
			[]string{"session_id", "selector"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID                    string `json:"session_id"`
					Selector, Value, Label, Goal string
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				return s.Select(ctx, id, in.Selector, in.Value, in.Label, in.Goal)
			}),
		writeTool("local_cobrowse_key", "Press one key: Enter, Tab, Escape, Backspace, Delete, arrows, Home, End, PageUp, PageDown, a letter, digit or space."+writeNote,
			map[string]any{"session_id": strProp(descSession), "key": strProp("The key name."), "goal": strProp(descGoal)},
			[]string{"session_id", "key"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID string `json:"session_id"`
					Key, Goal string
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				return s.Key(ctx, id, in.Key, in.Goal)
			}),
		writeTool("local_cobrowse_scroll", "Scroll with the mouse wheel at an element or viewport point."+writeNote,
			map[string]any{"session_id": strProp(descSession), "selector": strProp("CSS selector; wins over x/y."),
				"x": numProp("Viewport X."), "y": numProp("Viewport Y."),
				"delta_x": numProp("Horizontal scroll in pixels."), "delta_y": numProp("Vertical scroll in pixels (positive scrolls down)."), "goal": strProp(descGoal)},
			[]string{"session_id"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in scrollArgs
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				return s.Scroll(ctx, id, in.Selector, in.X, in.Y, in.DeltaX, in.DeltaY, in.Goal)
			}),
		writeTool("local_cobrowse_extract", "Read an element's text (bounded; truncated is true when cut) and optional attributes. The value of a password field is never returned."+readNote,
			map[string]any{"session_id": strProp(descSession), "selector": strProp("CSS selector."),
				"attrs":     strProp("Optional comma-separated attribute names, e.g. \"href,id\"."),
				"max_chars": numProp(fmt.Sprintf("Maximum text characters (default %d, at most %d).", localcobrowse.DefaultExtractChars, localcobrowse.MaxExtractChars)),
				"goal":      strProp(descGoal)},
			[]string{"session_id", "selector"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID string  `json:"session_id"`
					Selector  string  `json:"selector"`
					Attrs     string  `json:"attrs"`
					MaxChars  float64 `json:"max_chars"`
					Goal      string  `json:"goal"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				var attrs []string
				for _, a := range strings.Split(in.Attrs, ",") {
					if a = strings.TrimSpace(a); a != "" {
						attrs = append(attrs, a)
					}
				}
				res, err := s.Extract(ctx, id, in.Selector, attrs, int(in.MaxChars), in.Goal)
				if err != nil {
					return nil, err
				}
				return map[string]any{"text": res.Text, "attrs": res.Attrs, "truncated": res.Truncated}, nil
			}),
		{
			Name:        "local_cobrowse_screenshot",
			Description: "Capture the session's current viewport as a PNG image." + readNote,
			InputSchema: objSchema(map[string]any{"session_id": strProp(descSession), "goal": strProp(descGoal)}, "session_id"),
			Async:       true,
			CallContent: func(ctx context.Context, args json.RawMessage) ([]map[string]any, error) {
				s, err := svc()
				if err != nil {
					return nil, err
				}
				var in struct {
					SessionID string `json:"session_id"`
					Goal      string `json:"goal"`
				}
				if err := decodeArgs(args, &in); err != nil {
					recordArgRefusal(s, "local_cobrowse_screenshot", args, err)
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					recordArgRefusal(s, "local_cobrowse_screenshot", args, err)
					return nil, err
				}
				png, err := s.Screenshot(ctx, id, in.Goal)
				if err != nil {
					return nil, err
				}
				return []map[string]any{{"type": "image", "data": png, "mimeType": "image/png"}}, nil
			},
		},
		writeTool("local_cobrowse_handoff", "Hand the browser to the user, for example to log in or pay. The viewer shows them a one-line message from reason. Every browser action of yours is refused until they press Hand back; call local_cobrowse_wait with until=resumed.",
			map[string]any{"session_id": strProp(descSession), "reason": strProp("Short instruction shown to the user, e.g. \"log in, then hand back\"."), "goal": strProp(descGoal)},
			[]string{"session_id"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID    string `json:"session_id"`
					Reason, Goal string
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				return s.Handoff(ctx, id, in.Reason, in.Goal)
			}),
		writeTool("local_cobrowse_resume", "Confirm the browser is back with you. Only the user can hand control back (Hand back to agent in the viewer); while they have control this is refused, so call local_cobrowse_wait with until=resumed instead.",
			map[string]any{"session_id": strProp(descSession)}, []string{"session_id"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID string `json:"session_id"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				return s.Resume(ctx, id)
			}),
		writeTool("local_cobrowse_stop", "End a session. A throwaway session's cookies and logins are deleted; a saved-logins profile is encrypted again and kept.",
			map[string]any{"session_id": strProp(descSession)}, []string{"session_id"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID string `json:"session_id"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				if err := s.Stop(ctx, id); err != nil {
					return nil, err
				}
				return map[string]any{"session_id": id, "stopped": true}, nil
			}),
		writeTool("local_cobrowse_wait", "Wait for the user: until=approved (they approved AI actions on the site), until=commit (they answered a send/post/submit/pay request; approved means repeat the same call, denied is final), until=resumed (they handed control back) or until=unlocked (they unlocked a saved-logins profile; a detail says if they chose a throwaway session instead). Returns within timeout_seconds (at most 50). If still_waiting is true, call again; denied or revoked are final answers.",
			map[string]any{"session_id": strProp(descSession),
				"until":           map[string]any{"type": "string", "enum": []string{"approved", "resumed", "unlocked", "commit"}},
				"timeout_seconds": numProp("Seconds to wait, at most 50 (default 50).")},
			[]string{"session_id", "until"},
			func(ctx context.Context, s *localcobrowse.Service, args json.RawMessage) (any, error) {
				var in struct {
					SessionID string  `json:"session_id"`
					Until     string  `json:"until"`
					Timeout   float64 `json:"timeout_seconds"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				id, err := requireSession(in.SessionID)
				if err != nil {
					return nil, err
				}
				return s.Wait(ctx, id, in.Until, time.Duration(in.Timeout*float64(time.Second)))
			}),
	}
}

// closeOnSignal waits for the first signal on sigs, closes rt (which waits
// for any teardown already running, such as the deferred close after stdin
// EOF) and then exits. Later signals are absorbed while it closes, so a
// second Ctrl-C or SIGTERM cannot cut a profile seal short.
func closeOnSignal(rt *localCobrowseRuntime, sigs <-chan os.Signal, exit func(int)) {
	<-sigs
	rt.close()
	exit(0)
}

// handleLocalCobrowseSignals stops every co-browse session, re-sealing saved
// logins, before the process exits on SIGINT or SIGTERM. Both MCP entry
// points that host local co-browse install it.
func handleLocalCobrowseSignals(rt *localCobrowseRuntime) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go closeOnSignal(rt, sigs, os.Exit)
}
