package platform

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// editFake wires a latencyCDP so the edit scripts answer as a page would, and
// records the order of edit steps and input events.
type editFake struct {
	*latencyCDP
	mu    sync.Mutex
	steps []string
	// focus, selectRes and empty are what the page answers for each step.
	focus, selectRes, empty map[string]any
	// keys records every key event's params.
	keys []map[string]any
}

func newEditFake(t *testing.T) (*editFake, *CobrowseSessionManager, string) {
	t.Helper()
	e := &editFake{
		focus:     map[string]any{"ok": true, "tag": "input"},
		selectRes: map[string]any{"ok": true},
		empty:     map[string]any{"empty": true},
	}
	e.latencyCDP = startLatencyCDP(t, func(f *latencyCDP) {
		f.evalHook = func(expr string) (any, bool) {
			switch {
			case strings.Contains(expr, "/*edit:focus*/"):
				return e.focus, true
			case strings.Contains(expr, "/*edit:select*/"):
				return e.selectRes, true
			case strings.Contains(expr, "/*edit:empty*/"):
				return e.empty, true
			}
			return nil, false
		}
	})
	e.latencyCDP.mu.Lock()
	prev := e.latencyCDP.onCall
	e.latencyCDP.onCall = func(method string, params map[string]any) {
		if prev != nil {
			prev(method, params)
		}
		e.record(method, params)
	}
	e.latencyCDP.mu.Unlock()
	m, id := newBudgetSession(t, e.latencyCDP)
	t.Cleanup(func() { _ = m.Stop(id) })
	return e, m, id
}

func (e *editFake) record(method string, params map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch method {
	case "Runtime.evaluate":
		expr, _ := params["expression"].(string)
		for _, k := range []string{"focus", "select", "empty"} {
			if strings.Contains(expr, "/*edit:"+k+"*/") {
				e.steps = append(e.steps, "edit:"+k)
			}
		}
	case "Input.insertText":
		e.steps = append(e.steps, "insert")
	case "Input.dispatchKeyEvent":
		typ, _ := params["type"].(string)
		key, _ := params["key"].(string)
		e.steps = append(e.steps, typ+":"+key)
		e.keys = append(e.keys, params)
	}
}

func (e *editFake) got() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.steps, ",")
}

func TestTypeWithStepOrderReplace(t *testing.T) {
	e, m, id := newEditFake(t)
	st, err := m.TypeWith(id, "new value", TypeOptions{Selector: "#f", Clear: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.got(); got != "edit:focus,edit:select,insert" {
		t.Fatalf("steps = %s", got)
	}
	ef := st.Effects
	if ef == nil || !ef.Focused || ef.FocusTag != "input" || !ef.Cleared || ef.TypedLength != 9 || ef.TypedUnconfirmed {
		t.Fatalf("effects = %+v", ef)
	}
	if st.InputEdit != InputEditV1 {
		t.Fatalf("input_edit = %q", st.InputEdit)
	}
}

func TestTypeWithSelectorOnlyAndClearOnly(t *testing.T) {
	e, m, id := newEditFake(t)
	if _, err := m.TypeWith(id, "x", TypeOptions{Selector: "#f"}); err != nil {
		t.Fatal(err)
	}
	if got := e.got(); got != "edit:focus,insert" {
		t.Fatalf("selector only: %s", got)
	}
	e2, m2, id2 := newEditFake(t)
	if _, err := m2.TypeWith(id2, "x", TypeOptions{Clear: true}); err != nil {
		t.Fatal(err)
	}
	if got := e2.got(); got != "edit:select,insert" {
		t.Fatalf("clear only: %s", got)
	}
}

func TestTypeWithPlainIsOneInsert(t *testing.T) {
	e, m, id := newEditFake(t)
	if _, err := m.Type(id, "hi"); err != nil {
		t.Fatal(err)
	}
	if got := e.got(); got != "insert" {
		t.Fatalf("steps = %s", got)
	}
}

func TestTypeWithEmptyClearDeletesAndVerifies(t *testing.T) {
	e, m, id := newEditFake(t)
	st, err := m.TypeWith(id, "", TypeOptions{Selector: "#f", Clear: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.got(); got != "edit:focus,edit:select,rawKeyDown:Backspace,keyUp:Backspace,edit:empty" {
		t.Fatalf("steps = %s", got)
	}
	if !st.Effects.Cleared || st.Effects.TypedLength != 0 {
		t.Fatalf("effects = %+v", st.Effects)
	}
}

func TestTypeWithEmptyClearThatLeavesContentFails(t *testing.T) {
	e, m, id := newEditFake(t)
	e.empty = map[string]any{"empty": false}
	_, err := m.TypeWith(id, "", TypeOptions{Selector: "#f", Clear: true})
	ae, ok := AsActionIncomplete(err)
	if !ok {
		t.Fatalf("err = %v, want incomplete", err)
	}
	if ae.Effects.Cleared || !ae.Effects.Dispatched || !ae.Effects.Focused {
		t.Fatalf("effects = %+v", ae.Effects)
	}
}

func TestTypeWithValidationRefusalsDispatchNothing(t *testing.T) {
	e, m, id := newEditFake(t)
	long := strings.Repeat("a", MaxCobrowseSelectorLen+1)
	cases := []struct {
		name string
		text string
		opts TypeOptions
		want error
	}{
		{"empty text without clear", "", TypeOptions{Selector: "#f"}, ErrTypeEmptyText},
		{"empty text, no options", "", TypeOptions{}, ErrTypeEmptyText},
		{"over-long selector", "x", TypeOptions{Selector: long, Clear: true}, ErrSelectorTooLong},
	}
	for _, c := range cases {
		if _, err := m.TypeWith(id, c.text, c.opts); !errors.Is(err, c.want) {
			t.Fatalf("%s: err = %v", c.name, err)
		}
	}
	for _, mod := range []int{-1, 16, 99} {
		if _, err := m.KeyWith(id, "a", mod); !errors.Is(err, ErrModifiersOutRange) {
			t.Fatalf("modifiers %d: err = %v", mod, err)
		}
	}
	if got := e.got(); got != "" {
		t.Fatalf("a refusal dispatched: %s", got)
	}
	if n := e.dials.Load(); n != 0 {
		t.Fatalf("a refusal opened %d connections", n)
	}
}

func TestTypeWithSelectorRefusalsTypeNothing(t *testing.T) {
	for name, res := range map[string]map[string]any{
		"matches nothing":    {"error": "no element matches the selector"},
		"not editable":       {"error": "the selector does not match an editable field"},
		"did not take focus": {"error": "the field did not take focus"},
	} {
		t.Run(name, func(t *testing.T) {
			e, m, id := newEditFake(t)
			e.focus = res
			_, err := m.TypeWith(id, "secret", TypeOptions{Selector: "#nope", Clear: true})
			if err == nil {
				t.Fatal("want a refusal")
			}
			if _, inc := AsActionIncomplete(err); inc {
				t.Fatalf("a refusal that changed nothing must stay plain: %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks the text: %v", err)
			}
			if got := e.got(); got != "edit:focus" {
				t.Fatalf("steps after a refusal = %s", got)
			}
		})
	}
}

func TestTypeWithClearOfNonEditableFocusRefuses(t *testing.T) {
	e, m, id := newEditFake(t)
	e.selectRes = map[string]any{"error": "the focused element is not an editable field"}
	_, err := m.TypeWith(id, "x", TypeOptions{Clear: true})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if _, inc := AsActionIncomplete(err); inc {
		t.Fatalf("nothing changed, so the refusal stays plain: %v", err)
	}
	if got := e.got(); got != "edit:select" {
		t.Fatalf("steps = %s", got)
	}
}

// A handoff while a step is awaited stops every later step: nothing after it is
// dispatched, and the report says what had already happened.
func TestTypeWithHandoffBetweenStepsStopsTheAction(t *testing.T) {
	cases := []struct {
		name      string
		after     string // the step during which control is handed off
		opts      TypeOptions
		text      string
		wantSteps string
		focused   bool
	}{
		{"after focus", "edit:focus", TypeOptions{Selector: "#f", Clear: true}, "x", "edit:focus", true},
		{"after select", "edit:select", TypeOptions{Selector: "#f", Clear: true}, "x", "edit:focus,edit:select", true},
		{"after key down", "rawKeyDown:Backspace", TypeOptions{Selector: "#f", Clear: true}, "", "edit:focus,edit:select,rawKeyDown:Backspace", true},
		{"after key up", "keyUp:Backspace", TypeOptions{Clear: true}, "", "edit:select,rawKeyDown:Backspace,keyUp:Backspace", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, m, id := newEditFake(t)
			var once sync.Once
			e.latencyCDP.mu.Lock()
			prev := e.latencyCDP.onCall
			e.latencyCDP.onCall = func(method string, params map[string]any) {
				prev(method, params)
				e.mu.Lock()
				n := len(e.steps)
				last := ""
				if n > 0 {
					last = e.steps[n-1]
				}
				e.mu.Unlock()
				if last == c.after {
					once.Do(func() { forceHandoff(m, id) })
				}
			}
			e.latencyCDP.mu.Unlock()
			_, err := m.TypeWith(id, c.text, c.opts)
			ae, ok := AsActionIncomplete(err)
			if !ok || errors.Is(err, ErrHandedOff) {
				t.Fatalf("err = %v, want *ActionIncompleteError not wrapping ErrHandedOff", err)
			}
			if got := e.got(); got != c.wantSteps {
				t.Fatalf("steps = %s, want %s", got, c.wantSteps)
			}
			if ae.Effects.Focused != c.focused || ae.Effects.Cleared || !ae.Effects.Dispatched {
				t.Fatalf("effects = %+v", ae.Effects)
			}
			if ae.Effects.TypedLength != 0 {
				t.Fatalf("typed %d characters after the handoff", ae.Effects.TypedLength)
			}
		})
	}
}

func TestKeyEventParamsModifiers(t *testing.T) {
	// No modifiers: exactly what Key always sent.
	down := keyEventParams("down", "a", 0)
	if down["type"] != "keyDown" || down["text"] != "a" || down["modifiers"] != nil || down["commands"] != nil {
		t.Fatalf("plain a = %v", down)
	}
	if up := keyEventParams("up", "a", 0); up["type"] != "keyUp" || up["text"] != nil || up["modifiers"] != nil {
		t.Fatalf("plain up = %v", up)
	}
	if enter := keyEventParams("down", "Enter", 0); enter["type"] != "rawKeyDown" || enter["text"] != nil {
		t.Fatalf("enter = %v", enter)
	}
	// A chord produces no text and carries the mask on both events.
	for _, mod := range []int{ModCtrl, ModMeta, ModAlt, ModCtrl | ModShift} {
		d, u := keyEventParams("down", "a", mod), keyEventParams("up", "a", mod)
		if d["type"] != "rawKeyDown" || d["text"] != nil || d["modifiers"] != mod || u["modifiers"] != mod || u["text"] != nil {
			t.Fatalf("mod %d: down=%v up=%v", mod, d, u)
		}
	}
	if c, _ := keyEventParams("down", "a", ModCtrl)["commands"].([]string); len(c) != 1 || c[0] != "selectAll" {
		t.Fatalf("ctrl+a commands = %v", c)
	}
	if c, _ := keyEventParams("down", "A", ModMeta)["commands"].([]string); len(c) != 1 || c[0] != "selectAll" {
		t.Fatalf("meta+A commands = %v", c)
	}
	if keyEventParams("down", "b", ModCtrl)["commands"] != nil || keyEventParams("down", "a", ModAlt)["commands"] != nil {
		t.Fatal("only ctrl/meta+a carries selectAll")
	}
	// Shift alone still types, in capitals.
	if s := keyEventParams("down", "a", ModShift); s["type"] != "keyDown" || s["text"] != "A" || s["modifiers"] != ModShift {
		t.Fatalf("shift+a = %v", s)
	}
}

func TestKeyWithSendsModifiersAndNoText(t *testing.T) {
	e, m, id := newEditFake(t)
	if _, err := m.KeyWith(id, "a", ModCtrl); err != nil {
		t.Fatal(err)
	}
	if got := e.got(); got != "rawKeyDown:a,keyUp:a" {
		t.Fatalf("steps = %s", got)
	}
	for _, k := range e.keys {
		if k["text"] != nil || k["modifiers"] != float64(ModCtrl) {
			t.Fatalf("event = %v", k)
		}
	}
}

func TestStatusReportsInputEdit(t *testing.T) {
	_, m, id := newEditFake(t)
	st, ok := m.SessionStatus(id)
	if !ok || st.InputEdit != "type_clear_key_modifiers_v1" {
		t.Fatalf("status input_edit = %q", st.InputEdit)
	}
}
