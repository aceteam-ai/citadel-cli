package laneop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const sentinel = "SENTINEL-lane-message-7c1f"

var clock = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func newService(t *testing.T, f *fakeTmux) (*Service, *[]string) {
	t.Helper()
	var audit []string
	return &Service{
		Exec:   f.exec,
		Ledger: NewLedger(t.TempDir()),
		Now:    func() time.Time { return clock },
		Sleep:  func(context.Context, time.Duration) error { return nil },
		Audit: func(format string, args ...any) {
			audit = append(audit, strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")))
		},
	}, &audit
}

func request(message string) SendRequest {
	return SendRequest{Lane: "lane-a", LeaseID: "lease-1", NodeID: "42", Pane: "%7", OfferID: "offer-1", Message: message}
}

func TestSendSubmitsOnceThroughBracketedPaste(t *testing.T) {
	f := newFakeTmux()
	s, audit := newService(t, f)
	message := sentinel + "\nsecond line"
	got := s.Send(context.Background(), request(message))
	if got.Status != StatusSubmitted {
		t.Fatalf("status %+v, want submitted", got)
	}
	if p := f.panes["%7"]; len(p.submits) != 1 || p.submits[0] != message {
		t.Fatalf("submits %q", p.submits)
	}
	// The message only travels on stdin; a newline never becomes send-keys.
	for _, call := range f.calls {
		for _, arg := range call {
			if strings.Contains(arg, sentinel) {
				t.Fatalf("message on tmux argv: %q", call)
			}
		}
		if call[0] == "send-keys" && !slices.Equal(call, []string{"send-keys", "-t", "%7", "Enter"}) {
			t.Fatalf("send-keys carried more than Enter: %q", call)
		}
		if call[0] == "paste-buffer" && !(slices.Contains(call, "-p") && slices.Contains(call, "-r") && slices.Contains(call, "-d")) {
			t.Fatalf("paste is not bracketed, literal and self-deleting: %q", call)
		}
	}
	if len(f.stdins) != 1 || string(f.stdins[0]) != message {
		t.Fatalf("stdin %q", f.stdins)
	}
	if len(*audit) != 1 || strings.Contains((*audit)[0], sentinel) || !strings.Contains((*audit)[0], "offer=offer-1 outcome=submitted") {
		t.Fatalf("audit %q", *audit)
	}
	raw, err := os.ReadFile(filepath.Join(s.Ledger.dir, LedgerFileName))
	if err != nil || strings.Contains(string(raw), sentinel) {
		t.Fatalf("ledger holds the message or is missing: %v %s", err, raw)
	}
}

func TestTwoServicesSharingPaneDoNotInterleaveOffers(t *testing.T) {
	f := newFakeTmux()
	first, _ := newService(t, f)
	second := &Service{Exec: f.exec, Ledger: NewLedger(first.Ledger.dir), Now: first.Now, Sleep: first.Sleep}
	pasted := make(chan struct{})
	release := make(chan struct{})
	f.onPaste = func(*fakeTmux) {
		f.onPaste = nil
		close(pasted)
		<-release
	}
	firstResult := make(chan SendResult, 1)
	go func() { firstResult <- first.Send(context.Background(), request("first offer")) }()
	select {
	case <-pasted:
	case <-time.After(2 * time.Second):
		t.Fatal("first send never reached paste")
	}
	secondRequest := request("second offer")
	secondRequest.OfferID = "offer-2"
	secondResult := make(chan SendResult, 1)
	go func() { secondResult <- second.Send(context.Background(), secondRequest) }()
	select {
	case result := <-secondResult:
		t.Fatalf("second service crossed the pane lock during paste: %+v", result)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if got := <-firstResult; got.Status != StatusSubmitted {
		t.Fatalf("first send: %+v", got)
	}
	if got := <-secondResult; got.Status != StatusSubmitted {
		t.Fatalf("second send: %+v", got)
	}
	if got := f.panes["%7"].submits; !slices.Equal(got, []string{"first offer", "second offer"}) {
		t.Fatalf("interleaved pane submissions: %q", got)
	}
}

func TestSendDedupesOfferAcrossRedeliveryWithNoTmuxCalls(t *testing.T) {
	f := newFakeTmux()
	s, _ := newService(t, f)
	if got := s.Send(context.Background(), request(sentinel)); got.Status != StatusSubmitted {
		t.Fatalf("first %+v", got)
	}
	// A restarted worker: same ledger directory, fresh service and process state.
	again := &Service{Exec: f.exec, Ledger: NewLedger(s.Ledger.dir), Now: s.Now, Sleep: s.Sleep}
	before := len(f.calls)
	got := again.Send(context.Background(), request(sentinel))
	if got.Status != StatusSubmitted || !got.Replayed {
		t.Fatalf("replay %+v", got)
	}
	if len(f.calls) != before || len(f.panes["%7"].submits) != 1 {
		t.Fatalf("redelivery touched tmux: %v", f.commands()[before:])
	}
	other := request("different text")
	if got := again.Send(context.Background(), other); got.Status != StatusRejected || got.Reason != ReasonOfferMismatch {
		t.Fatalf("mismatch %+v", got)
	}
}

func TestSendPendingRecordReadsUnknownWithoutTyping(t *testing.T) {
	f := newFakeTmux()
	s, _ := newService(t, f)
	req := request(sentinel)
	if _, _, err := s.Ledger.Reserve(req.OfferID, Entry{Fingerprint: Fingerprint(req), Status: statusPending}, clock); err != nil {
		t.Fatal(err)
	}
	got := s.Send(context.Background(), req)
	if got.Status != StatusUnknown || len(f.calls) != 0 {
		t.Fatalf("got %+v calls %v", got, f.commands())
	}
}

func TestSendRefusals(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*fakeTmux)
		reason string
	}{
		{"draft", func(f *fakeTmux) { f.panes["%7"].draft = "half-typed by a human" }, ReasonHumanDraftPending},
		{"unknown screen", func(f *fakeTmux) { f.panes["%7"].screen = "user@host:~$ \n" }, ReasonUnknownScreen},
		{"dialog", func(f *fakeTmux) { f.panes["%7"].dialog = true }, ReasonHumanPromptOpen},
		{"copy mode", func(f *fakeTmux) { f.panes["%7"].inMode = true }, ReasonHumanDriving},
		{"recent key press", func(f *fakeTmux) { f.activity = []int64{clock.Add(-10 * time.Second).Unix()} }, ReasonHumanDriving},
		{"unknown pane", func(f *fakeTmux) { f.panes["%9"] = f.panes["%7"]; delete(f.panes, "%7") }, ReasonPaneNotFound},
		{"listed under another id", func(f *fakeTmux) { f.panes["%7"].listedAs = "%70" }, ReasonPaneNotFound},
		{"citadel terminal session", func(f *fakeTmux) { f.panes["%7"].managed = true }, ReasonPaneNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTmux()
			tc.set(f)
			s, _ := newService(t, f)
			got := s.Send(context.Background(), request(sentinel))
			if got.Status != StatusRejected || got.Reason != tc.reason {
				t.Fatalf("got %+v, want rejected %s", got, tc.reason)
			}
			for _, cmd := range f.commands() {
				if cmd == "load-buffer" || cmd == "paste-buffer" || cmd == "send-keys" {
					t.Fatalf("typed into a refused pane: %v", f.commands())
				}
			}
			// The refusal is durable: a redelivery replays it.
			if again := s.Send(context.Background(), request(sentinel)); again.Status != StatusRejected || !again.Replayed {
				t.Fatalf("replay %+v", again)
			}
		})
	}
}

func TestSendOldKeyPressDoesNotRefuse(t *testing.T) {
	f := newFakeTmux()
	f.activity = []int64{clock.Add(-2 * HumanActivityWindow).Unix()}
	s, _ := newService(t, f)
	if got := s.Send(context.Background(), request(sentinel)); got.Status != StatusSubmitted {
		t.Fatalf("got %+v", got)
	}
}

func TestSendNoEnterWhenPasteVerificationFails(t *testing.T) {
	cases := map[string]func(*fakeTmux){
		"human typed after the paste": func(f *fakeTmux) {
			f.onPaste = func(f *fakeTmux) { f.activity = []int64{clock.Unix()} }
		},
		"box does not hold the message": func(f *fakeTmux) {
			f.onPaste = func(f *fakeTmux) { f.panes["%7"].draft += " and more" }
		},
		"dialog opened": func(f *fakeTmux) {
			f.onPaste = func(f *fakeTmux) { f.panes["%7"].dialog = true }
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeTmux()
			set(f)
			s, _ := newService(t, f)
			got := s.Send(context.Background(), request(sentinel))
			if got.Status != StatusUnknown {
				t.Fatalf("got %+v", got)
			}
			if slices.Contains(f.commands(), "send-keys") {
				t.Fatalf("pressed Enter after a failed check: %v", f.commands())
			}
		})
	}
}

func TestSendUnconsumedEnterIsUnknown(t *testing.T) {
	f := newFakeTmux()
	f.ignoreEnter = true
	s, _ := newService(t, f)
	if got := s.Send(context.Background(), request(sentinel)); got.Status != StatusUnknown {
		t.Fatalf("got %+v", got)
	}
	if n := strings.Count(strings.Join(f.commands(), ","), "send-keys"); n != 1 {
		t.Fatalf("Enter sent %d times", n)
	}
}

func TestSendUnreadableLedgerNeverTypes(t *testing.T) {
	f := newFakeTmux()
	s, _ := newService(t, f)
	if err := os.WriteFile(filepath.Join(s.Ledger.dir, LedgerFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.Send(context.Background(), request(sentinel)); got.Status != StatusUnknown || len(f.calls) != 0 {
		t.Fatalf("got %+v calls %v", got, f.commands())
	}
	raw, _ := os.ReadFile(filepath.Join(s.Ledger.dir, LedgerFileName))
	if string(raw) != "{not json" {
		t.Fatal("damaged ledger was rewritten")
	}
}

func TestSendFullLedgerRefuses(t *testing.T) {
	f := newFakeTmux()
	s, _ := newService(t, f)
	s.Ledger.cap = 1
	if _, _, err := s.Ledger.Reserve("other", Entry{Fingerprint: "x", Status: StatusSubmitted}, clock); err != nil {
		t.Fatal(err)
	}
	if got := s.Send(context.Background(), request(sentinel)); got.Status != StatusRejected || got.Reason != ReasonLedgerFull {
		t.Fatalf("got %+v", got)
	}
	if slices.Contains(f.commands(), "paste-buffer") {
		t.Fatal("typed with a full ledger")
	}
}

func TestLedgerPrunesSettledButKeepsPending(t *testing.T) {
	l := NewLedger(t.TempDir())
	old := clock.Add(-2 * DefaultLedgerTTL)
	l.Reserve("settled", Entry{Fingerprint: "a", Status: StatusSubmitted}, old)
	l.Reserve("pending", Entry{Fingerprint: "b", Status: statusPending}, old)
	l.Reserve("new", Entry{Fingerprint: "c", Status: StatusRejected}, clock)
	if _, ok, _ := l.Lookup("settled"); ok {
		t.Fatal("expired settled record kept")
	}
	if _, ok, _ := l.Lookup("pending"); !ok {
		t.Fatal("pending record pruned")
	}
	if NewLedger("relative/dir") != nil || NewLedger("") != nil {
		t.Fatal("ledger accepted a non-absolute directory")
	}
}

func TestCaptureBoundedScrubbedAndBusy(t *testing.T) {
	f := newFakeTmux()
	key := "ghp_" + strings.Repeat("A", 36)
	long := strings.Repeat("é line of output\n", 900)
	f.panes["%7"].screen = long + "token " + key + "\npasscode 4931-7788\n" + rule + "\n> \n" + rule + "\n"
	s, _ := newService(t, f)
	got, err := s.Capture(context.Background(), "%7", 4096, "4931-7788")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tail) > 4096 || !utf8.ValidString(got.Tail) {
		t.Fatalf("tail %d bytes", len(got.Tail))
	}
	if strings.Contains(got.Tail, key) || strings.Contains(got.Tail, "4931-7788") || !strings.Contains(got.Tail, "[REDACTED]") {
		t.Fatalf("tail not scrubbed: %q", got.Tail[len(got.Tail)-200:])
	}
	if got.Busy == nil || *got.Busy || got.Evidence != "prompt_visible" {
		t.Fatalf("busy %v %s", got.Busy, got.Evidence)
	}
	if _, err := time.Parse(time.RFC3339, got.CapturedAt); err != nil || !strings.HasSuffix(got.CapturedAt, "Z") {
		t.Fatalf("captured_at %q", got.CapturedAt)
	}
	f.panes["%7"].screen = ""
	f.panes["%7"].busy = true
	got, _ = s.Capture(context.Background(), "%7", 100)
	if got.Busy == nil || !*got.Busy || got.Evidence != "activity_recent" {
		t.Fatalf("busy %v %s", got.Busy, got.Evidence)
	}
	f.panes["%7"].screen = "plain shell $ "
	got, _ = s.Capture(context.Background(), "%7", 100)
	if got.Busy != nil || got.Evidence != "none" || got.Map()["pane_busy"] != nil {
		t.Fatalf("unknown screen reported busy %v", got.Busy)
	}
}

func TestCaptureSecretAcrossTheCutIsNeverPartlyExposed(t *testing.T) {
	f := newFakeTmux()
	key := "ghp_" + strings.Repeat("B", 36)
	f.panes["%7"].screen = "x" + key + "\n" + strings.Repeat("y", 30) + "\n"
	s, _ := newService(t, f)
	got, err := s.Capture(context.Background(), "%7", 40)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Tail, "BBBB") {
		t.Fatalf("partial secret in tail %q", got.Tail)
	}
}

func TestCaptureRefusesUnknownAndForeignPanes(t *testing.T) {
	for _, id := range []string{"%8", "7", "%7;kill-server", ""} {
		f := newFakeTmux()
		s, _ := newService(t, f)
		if _, err := s.Capture(context.Background(), id, 100); refusalReason(err) != ReasonPaneNotFound {
			t.Fatalf("%q: %v", id, err)
		}
		if slices.Contains(f.commands(), "capture-pane") {
			t.Fatalf("%q captured", id)
		}
	}
	f := newFakeTmux()
	f.failOn = "list-panes"
	s, _ := newService(t, f)
	if _, err := s.Capture(context.Background(), "%7", 100); refusalReason(err) != ReasonTmuxUnavailable {
		t.Fatalf("no server: %v", err)
	}
	if _, err := s.Capture(context.Background(), "%7", MaxCaptureBytes+1); refusalReason(err) != "invalid_bound" {
		t.Fatalf("bound: %v", err)
	}
}

func TestAvailableNeedsTmux(t *testing.T) {
	if Available(context.Background(), func(context.Context, []byte, ...string) ([]byte, error) { return nil, ErrTmuxUnavailable }) {
		t.Fatal("available without tmux")
	}
	if Available(context.Background(), nil) {
		t.Fatal("available with no exec")
	}
}

func TestMinimalEnvDropsTmuxRedirects(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin", "HOME": "/home/u", "TMUX": "/tmp/other,1,0", "TMUX_PANE": "%1", "TMUX_TMPDIR": "/tmp/x"}
	got := minimalEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if strings.Contains(strings.Join(got, " "), "TMUX") || len(got) != 2 {
		t.Fatalf("env %q", got)
	}
}

func TestValidMessage(t *testing.T) {
	for _, ok := range []string{"hi", "two\nlines\tand tab", strings.Repeat("a", MaxMessageBytes)} {
		if !ValidMessage(ok) {
			t.Fatalf("refused %q", ok)
		}
	}
	for _, bad := range []string{"", "esc\x1b[2J", "cr\r", "del\x7f", "c1\u0085", strings.Repeat("a", MaxMessageBytes+1), "\xff"} {
		if ValidMessage(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
