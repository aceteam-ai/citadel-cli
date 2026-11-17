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
	// focusRes is the RemoteObject the focus and target scripts answer with.
	focusRes map[string]any
	// selectRes and empty are what the bound select and empty steps answer;
	// verify is the bound verify answer.
	selectRes, empty, verify map[string]any
	// loaders is the loaderId returned by successive getFrameTree calls (the
	// last one repeats).
	loaders []string
	loaderN int
	// keys records every key event's params.
	keys []map[string]any
}

func newEditFake(t *testing.T, mutate ...func(*latencyCDP)) (*editFake, *CobrowseSessionManager, string) {
	t.Helper()
	e := &editFake{
		focusRes:  map[string]any{"type": "object", "subtype": "node", "objectId": "obj-1", "className": "HTMLInputElement"},
		selectRes: map[string]any{"ok": true},
		empty:     map[string]any{"empty": true},
		verify:    map[string]any{"ok": true},
		loaders:   []string{"L1"},
	}
	e.latencyCDP = startLatencyCDP(t, func(f *latencyCDP) {
		for _, mu := range mutate {
			mu(f)
		}
		f.evalHook = func(expr string) (any, bool) {
			if strings.Contains(expr, "/*edit:focus*/") || strings.Contains(expr, "/*edit:target*/") {
				e.mu.Lock()
				defer e.mu.Unlock()
				return rawRemote(e.focusRes), true
			}
			return nil, false
		}
		f.callHook = func(method string, params map[string]any) (map[string]any, bool) {
			e.mu.Lock()
			defer e.mu.Unlock()
			switch method {
			case "Page.getFrameTree":
				i := e.loaderN
				if i >= len(e.loaders) {
					i = len(e.loaders) - 1
				}
				e.loaderN++
				return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"loaderId": e.loaders[i]}}}, true
			case "Runtime.callFunctionOn":
				fn, _ := params["functionDeclaration"].(string)
				var v map[string]any
				switch {
				case strings.Contains(fn, "/*edit:select*/"):
					v = e.selectRes
				case strings.Contains(fn, "/*edit:verify*/"):
					v = e.verify
				case strings.Contains(fn, "/*edit:empty*/"):
					v = e.empty
				}
				return map[string]any{"result": map[string]any{"type": "object", "value": v}}, true
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
	marker := func(s string) {
		for _, k := range []string{"focus", "target", "select", "empty"} {
			if strings.Contains(s, "/*edit:"+k+"*/") {
				e.steps = append(e.steps, "edit:"+k)
			}
		}
	}
	switch method {
	case "Runtime.evaluate":
		expr, _ := params["expression"].(string)
		marker(expr)
	case "Runtime.callFunctionOn":
		fn, _ := params["functionDeclaration"].(string)
		marker(fn)
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
	if ef == nil || !ef.Focused || ef.FocusTag != "input" || !ef.Cleared || ef.ClearedUnconfirmed || ef.TypedLength != 9 || ef.TypedUnconfirmed {
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
	if got := e2.got(); got != "edit:target,edit:select,insert" {
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
	if !st.Effects.Cleared || st.Effects.ClearedUnconfirmed || st.Effects.TypedLength != 0 {
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

// A lost insert acknowledgement leaves the field possibly replaced: the report
// says so as unconfirmed, never as not cleared.
func TestTypeWithLostInsertAckReportsClearUnconfirmed(t *testing.T) {
	_, m, id := newEditFake(t, func(f *latencyCDP) { f.dropOn = map[string]bool{"Input.insertText": true} })
	_, err := m.TypeWith(id, "replacement", TypeOptions{Selector: "#f", Clear: true})
	ae, ok := AsActionIncomplete(err)
	if !ok {
		t.Fatalf("err = %v, want incomplete", err)
	}
	ef := ae.Effects
	if ef.Cleared || !ef.ClearedUnconfirmed || !ef.TypedUnconfirmed || ef.TypedLength != 11 || !ef.Dispatched {
		t.Fatalf("effects = %+v", ef)
	}
	if strings.Contains(err.Error(), "replacement") {
		t.Fatalf("error leaks the text: %v", err)
	}
	if !strings.Contains(err.Error(), "cleared the field (unconfirmed)") {
		t.Fatalf("error does not say the clear is unconfirmed: %v", err)
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

func TestCheckKeyChordAllowlist(t *testing.T) {
	allowed := []struct {
		key string
		mod int
	}{
		{"Enter", 0}, {"v", 0}, {"a", ModCtrl}, {"A", ModCtrl}, {"z", ModCtrl}, {"y", ModCtrl},
		{"a", ModMeta}, {"z", ModMeta}, {"y", ModMeta}, {"a", ModShift}, {"Tab", ModShift}, {"Delete", 0},
		{"ArrowLeft", ModCtrl}, {"Home", ModCtrl}, {"End", ModCtrl}, {"Backspace", ModCtrl}, {"Delete", ModCtrl},
		{"ArrowRight", ModAlt}, {"Backspace", ModAlt}, {"Delete", ModAlt}, {"ArrowUp", ModShift},
	}
	for _, c := range allowed {
		if err := CheckKeyChord(c.key, c.mod); err != nil {
			t.Errorf("%s+%d: %v", c.key, c.mod, err)
		}
	}
	refused := []struct {
		key string
		mod int
	}{
		{"v", ModCtrl}, {"c", ModCtrl}, {"x", ModCtrl}, {"v", ModMeta}, {"c", ModMeta}, {"x", ModMeta},
		{"Delete", ModShift}, {"a", ModAlt}, {"b", ModAlt}, {"a", ModCtrl | ModShift}, {"z", ModCtrl | ModShift},
		{"a", ModCtrl | ModAlt}, {"Home", ModMeta}, {"Enter", ModCtrl}, {"v", ModCtrl | ModShift},
		{"ArrowLeft", ModAlt | ModShift},
	}
	for _, c := range refused {
		if err := CheckKeyChord(c.key, c.mod); !errors.Is(err, ErrKeyChordNotAllowed) {
			t.Errorf("%s+%d: err = %v, want a refusal", c.key, c.mod, err)
		}
	}
}

func TestKeyWithRefusedChordDispatchesNothing(t *testing.T) {
	e, m, id := newEditFake(t)
	for _, c := range []struct {
		key string
		mod int
	}{{"v", ModCtrl}, {"c", ModCtrl}, {"x", ModMeta}, {"Delete", ModShift}, {"a", ModAlt}} {
		if _, err := m.KeyWith(id, c.key, c.mod); !errors.Is(err, ErrKeyChordNotAllowed) {
			t.Fatalf("%s+%d: err = %v", c.key, c.mod, err)
		}
	}
	if got := e.got(); got != "" || e.dials.Load() != 0 {
		t.Fatalf("a refused chord dispatched: %q (%d dials)", got, e.dials.Load())
	}
}

func TestTypeWithSelectorRefusalsTypeNothing(t *testing.T) {
	for name, msg := range map[string]string{
		"matches nothing": "no element matches the selector",
		"not editable":    "the selector does not match an editable field",
	} {
		t.Run(name, func(t *testing.T) {
			e, m, id := newEditFake(t)
			e.focusRes = map[string]any{"type": "string", "value": msg}
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

// focus() already ran when the field did not take it, so the action counts as
// dispatched and is reported, not refused as a no-op.
func TestTypeWithNoFocusAfterFocusRanIsDispatched(t *testing.T) {
	e, m, id := newEditFake(t)
	e.focusRes = map[string]any{"type": "string", "value": "!the field did not take focus"}
	_, err := m.TypeWith(id, "secret", TypeOptions{Selector: "#f", Clear: true})
	ae, ok := AsActionIncomplete(err)
	if !ok || !ae.Effects.Dispatched {
		t.Fatalf("err = %v, want an incomplete action with Dispatched", err)
	}
	if strings.Contains(err.Error(), "!") {
		t.Fatalf("marker leaked into the error: %v", err)
	}
	if got := e.got(); got != "edit:focus" {
		t.Fatalf("steps = %s", got)
	}
}

func TestTypeWithClearOfNonEditableFocusRefuses(t *testing.T) {
	e, m, id := newEditFake(t)
	e.focusRes = map[string]any{"type": "string", "value": "the focused element is not an editable field"}
	_, err := m.TypeWith(id, "x", TypeOptions{Clear: true})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if _, inc := AsActionIncomplete(err); inc {
		t.Fatalf("nothing changed, so the refusal stays plain: %v", err)
	}
	if got := e.got(); got != "edit:target" {
		t.Fatalf("steps = %s", got)
	}
}

// A selection that is not the whole content refuses with nothing typed.
func TestTypeWithPartialSelectionRefuses(t *testing.T) {
	e, m, id := newEditFake(t)
	e.selectRes = map[string]any{"error": "the selection does not span the whole field"}
	_, err := m.TypeWith(id, "x", TypeOptions{Selector: "#f", Clear: true})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if got := e.got(); got != "edit:focus,edit:select" {
		t.Fatalf("steps = %s", got)
	}
}

// Focus moving away, or the page navigating, between the steps and the
// destructive event refuses with ErrTargetChanged and types nothing.
func TestTypeWithTargetChangedBeforeInsertTypesNothing(t *testing.T) {
	cases := map[string]func(e *editFake){
		"focus moved":    func(e *editFake) { e.verify = map[string]any{"ok": false} },
		"page navigated": func(e *editFake) { e.loaders = []string{"L1", "L2"} },
	}
	for name, mutate := range cases {
		for _, text := range []string{"replacement", ""} {
			t.Run(name+"/text="+text, func(t *testing.T) {
				e, m, id := newEditFake(t)
				mutate(e)
				_, err := m.TypeWith(id, text, TypeOptions{Selector: "#f", Clear: true})
				if !errors.Is(err, ErrTargetChanged) {
					t.Fatalf("err = %v, want ErrTargetChanged", err)
				}
				if got := e.got(); got != "edit:focus,edit:select" {
					t.Fatalf("steps = %s (nothing may be typed or deleted)", got)
				}
			})
		}
	}
}

// A handoff while a step is awaited stops every later step: nothing after it is
// dispatched, and the report says what had already happened.
func TestTypeWithHandoffBetweenStepsStopsTheAction(t *testing.T) {
	cases := []struct {
		name        string
		after       string // the step during which control is handed off
		opts        TypeOptions
		text        string
		wantSteps   string
		focused     bool
		unconfirmed bool
	}{
		{"after focus", "edit:focus", TypeOptions{Selector: "#f", Clear: true}, "x", "edit:focus", true, false},
		{"after select", "edit:select", TypeOptions{Selector: "#f", Clear: true}, "x", "edit:focus,edit:select", true, false},
		{"after key down", "rawKeyDown:Backspace", TypeOptions{Selector: "#f", Clear: true}, "", "edit:focus,edit:select,rawKeyDown:Backspace", true, true},
		{"after key up", "keyUp:Backspace", TypeOptions{Clear: true}, "", "edit:target,edit:select,rawKeyDown:Backspace,keyUp:Backspace", false, true},
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
			if ae.Effects.Focused != c.focused || ae.Effects.Cleared || ae.Effects.ClearedUnconfirmed != c.unconfirmed || !ae.Effects.Dispatched {
				t.Fatalf("effects = %+v", ae.Effects)
			}
			if ae.Effects.TypedLength != 0 {
				t.Fatalf("typed %d characters after the handoff", ae.Effects.TypedLength)
			}
		})
	}
}

// The first fence: a handoff during the before-probe is a plain ErrHandedOff
// and nothing is dispatched, for the edit forms of type and for key chords.
func TestEditActionsHandoffDuringBeforeProbeSendNothing(t *testing.T) {
	actions := map[string]func(m *CobrowseSessionManager, id string) error{
		"type selector+clear": func(m *CobrowseSessionManager, id string) error {
			_, err := m.TypeWith(id, "x", TypeOptions{Selector: "#f", Clear: true})
			return err
		},
		"type clear": func(m *CobrowseSessionManager, id string) error {
			_, err := m.TypeWith(id, "", TypeOptions{Clear: true})
			return err
		},
		"key chord": func(m *CobrowseSessionManager, id string) error {
			_, err := m.KeyWith(id, "a", ModCtrl)
			return err
		},
	}
	for name, run := range actions {
		t.Run(name, func(t *testing.T) {
			e, m, id := newEditFake(t)
			e.latencyCDP.mu.Lock()
			prev := e.latencyCDP.onCall
			hand := handoffDuringProbe(m, id)
			e.latencyCDP.onCall = func(method string, params map[string]any) {
				prev(method, params)
				hand(method, params)
			}
			e.latencyCDP.mu.Unlock()
			err := run(m, id)
			if !errors.Is(err, ErrHandedOff) {
				t.Fatalf("err = %v, want ErrHandedOff", err)
			}
			if _, inc := AsActionIncomplete(err); inc {
				t.Fatalf("a refusal must stay plain: %v", err)
			}
			if got := e.got(); got != "" {
				t.Fatalf("dispatched after the handoff: %s", got)
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

// A page-tracking session (local mode) does not take the new fields, so it does
// not advertise them.
func TestTrackedSessionDoesNotReportInputEdit(t *testing.T) {
	f := startLatencyCDP(t, nil)
	m, id := newTrackedBudgetSession(t, f, nil)
	st, ok := m.SessionStatus(id)
	if !ok || st.InputEdit != "" {
		t.Fatalf("tracked status input_edit = %q", st.InputEdit)
	}
}
