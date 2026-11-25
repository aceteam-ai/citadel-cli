package laneop

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// fakeTmux is a scripted tmux server with one or more panes. It never execs.
// A pane's screen is rendered from its draft so a paste and an Enter change
// what the next capture sees, like a harness input box would.
type fakeTmux struct {
	mu       sync.Mutex
	panes    map[string]*fakePane
	calls    [][]string
	stdins   [][]byte
	buffers  map[string][]byte
	activity []int64
	failOn   string
	// ignoreEnter keeps the draft after Enter (an unconsumed submit).
	ignoreEnter bool
	// onPaste runs after a paste lands, for races such as a key press.
	onPaste func(*fakeTmux)
}

type fakePane struct {
	session  string
	draft    string
	screen   string // when set, overrides the rendered input box
	dialog   bool
	busy     bool
	inMode   bool
	managed  bool
	submits  []string
	listedAs string
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{panes: map[string]*fakePane{"%7": {session: "$2"}}, buffers: map[string][]byte{}}
}

const rule = "────────────────────────────────────────"

func (p *fakePane) render() string {
	if p.screen != "" {
		return p.screen
	}
	var b strings.Builder
	b.WriteString("Earlier output from the harness\n> an earlier prompt in the transcript\n\n")
	if p.busy {
		b.WriteString("✻ Working… (12s · esc to interrupt)\n\n")
	}
	if p.dialog {
		b.WriteString(" Do you want to proceed?\n ❯ 1. Yes\n   2. No\n")
		return b.String()
	}
	b.WriteString(rule + "\n")
	lines := strings.Split(p.draft, "\n")
	b.WriteString("> " + lines[0] + "\n")
	for _, line := range lines[1:] {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString(rule + "\n  ? for shortcuts\n")
	return b.String()
}

func (f *fakeTmux) exec(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if stdin != nil {
		f.stdins = append(f.stdins, append([]byte(nil), stdin...))
	}
	if len(args) == 0 {
		return nil, ErrTmuxUnavailable
	}
	if f.failOn == args[0] {
		return nil, ErrTmuxUnavailable
	}
	target := func() (*fakePane, error) {
		for i, a := range args {
			if a == "-t" && i+1 < len(args) {
				if p, ok := f.panes[args[i+1]]; ok {
					return p, nil
				}
			}
		}
		return nil, ErrTmuxUnavailable
	}
	switch args[0] {
	case "-V":
		return []byte("tmux 3.4\n"), nil
	case "list-panes":
		var out []string
		for id, p := range f.panes {
			if p.listedAs != "" {
				out = append(out, p.listedAs)
				continue
			}
			out = append(out, id)
		}
		return []byte(strings.Join(out, "\n") + "\n"), nil
	case "display-message":
		p, err := target()
		if err != nil {
			return nil, err
		}
		id := args[3]
		b := func(v bool) string {
			if v {
				return "1"
			}
			return "0"
		}
		return []byte(fmt.Sprintf("%s|%s|0|%s|0|0|%s\n", id, p.session, b(p.inMode), b(p.managed))), nil
	case "capture-pane":
		p, err := target()
		if err != nil {
			return nil, err
		}
		return []byte(p.render()), nil
	case "list-clients":
		var out []string
		for _, a := range f.activity {
			out = append(out, fmt.Sprint(a))
		}
		return []byte(strings.Join(out, "\n")), nil
	case "load-buffer":
		f.buffers[args[2]] = stdin
		return nil, nil
	case "delete-buffer":
		delete(f.buffers, args[2])
		return nil, nil
	case "paste-buffer":
		p, err := target()
		if err != nil {
			return nil, err
		}
		name := args[len(args)-3]
		p.draft += string(f.buffers[name])
		delete(f.buffers, name)
		if f.onPaste != nil {
			f.onPaste(f)
		}
		return nil, nil
	case "send-keys":
		p, err := target()
		if err != nil {
			return nil, err
		}
		if args[len(args)-1] != "Enter" {
			return nil, fmt.Errorf("unexpected keys")
		}
		if !f.ignoreEnter {
			p.submits = append(p.submits, p.draft)
			p.draft = ""
		}
		return nil, nil
	}
	return nil, ErrTmuxUnavailable
}

func (f *fakeTmux) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c[0])
	}
	return out
}
