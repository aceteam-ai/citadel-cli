package localcobrowse

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// el builds a resolved target on a synthetic local page.
func el(mod func(*platform.TargetInfo)) platform.TargetInfo {
	t := platform.TargetInfo{Resolved: true, Tag: "div", PageURL: "http://a.test/form"}
	if mod != nil {
		mod(&t)
	}
	return t
}

func TestClassify(t *testing.T) {
	sendBtn := el(func(t *platform.TargetInfo) {
		t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Send", true
	})
	cases := []struct {
		name string
		kind string
		key  string
		t    platform.TargetInfo
		want Class
	}{
		{"Send button", "click", "", sendBtn, ClassCommit},
		{"Post label in another language", "click", "", el(func(t *platform.TargetInfo) { t.Tag, t.Name, t.Actionable = "button", "Envoyer le message", true }), ClassCommit},
		{"Pay now", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Pay now", true
		}), ClassCommit},
		{"Place order", "click", "", el(func(t *platform.TargetInfo) { t.Tag, t.Name, t.Actionable = "button", "Place your order", true }), ClassCommit},
		{"submit input in a form", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable, t.InForm, t.FormMethod = "input", "submit", "Go", true, true, "post"
		}), ClassCommit},
		{"unlabeled button in a form defaults to submit", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable, t.InForm = "button", "submit", "Next", true, true
		}), ClassCommit},
		{"button type=button named Expand", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Expand menu", true
		}), ClassFill},
		{"search submit is fill", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable, t.InForm, t.Search = "button", "submit", "Search", true, true, true
		}), ClassFill},
		{"nameless actionable is commit", "click", "", el(func(t *platform.TargetInfo) { t.Tag, t.Actionable = "div", true }), ClassCommit},
		{"unresolved target is commit", "click", "", platform.TargetInfo{Reason: "target is inside iframe"}, ClassCommit},
		{"focus a text field", "click", "", el(func(t *platform.TargetInfo) { t.Tag, t.InputType, t.Entry = "input", "text", true }), ClassFill},
		{"tick a checkbox", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "input", "checkbox", "Remember me", true
		}), ClassFill},
		{"plain text click", "click", "", el(nil), ClassFill},
		{"same-site link", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.Name, t.Href, t.Actionable = "a", "Pricing", "http://a.test/pricing", true
		}), ClassNavigate},
		{"link to unsafe scheme", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.Name, t.Href, t.Actionable = "a", "Open", "javascript:void(0)", true
		}), ClassCommit},
		{"logout link", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.Name, t.Href, t.Actionable = "a", "Account", "http://a.test/logout", true
		}), ClassCommit},
		{"Enter in a composer", "key", "Enter", el(func(t *platform.TargetInfo) { t.Tag, t.Entry, t.Composer = "textarea", true, true }), ClassCommit},
		{"Enter in a form field", "key", "Enter", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Entry, t.InForm = "input", "text", true, true
		}), ClassCommit},
		{"Enter in a search box", "key", "Enter", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Entry, t.Search, t.InForm = "input", "search", true, true, true
		}), ClassFill},
		{"Enter on a focused Send button", "key", "Enter", sendBtn, ClassCommit},
		{"Space on a focused Send button", "key", " ", sendBtn, ClassCommit},
		{"Tab is fill", "key", "Tab", sendBtn, ClassFill},
		{"typing a letter is fill", "key", "a", el(func(t *platform.TargetInfo) { t.Tag, t.Entry = "textarea", true }), ClassFill},
		{"Enter with nothing focused", "key", "Enter", el(func(t *platform.TargetInfo) { t.Tag = "body" }), ClassFill},
		{"select that submits", "select", "", el(func(t *platform.TargetInfo) { t.Tag, t.AutoSubmit = "select", true }), ClassCommit},
		{"plain select", "select", "", el(func(t *platform.TargetInfo) { t.Tag = "select" }), ClassFill},
		{"type is fill", "type", "", el(func(t *platform.TargetInfo) { t.Tag, t.Entry = "textarea", true }), ClassFill},
		{"unresolved key is commit", "key", "a", platform.TargetInfo{}, ClassCommit},
		{"scroll is fill", "scroll", "", platform.TargetInfo{}, ClassFill},
	}
	for _, c := range cases {
		if got := Classify(c.kind, c.key, c.t); got.Class != c.want {
			t.Errorf("%s: got %s (%s), want %s", c.name, got.Class, got.Reason, c.want)
		}
	}
	if d := Classify("click", "", el(func(t *platform.TargetInfo) { t.Tag, t.Href, t.Actionable = "a", "http://www.other.test/x", true })); d.Domain != "other.test" {
		t.Errorf("navigate domain = %q", d.Domain)
	}
	if d := Classify("explode", "", el(nil)); d.Class != ClassCommit {
		t.Errorf("an unknown action kind must be a commit, got %s", d.Class)
	}
}

func TestRegistrableDomain(t *testing.T) {
	for in, want := range map[string]string{
		"http://www.example.com/x":      "example.com",
		"https://shop.example.com:8443": "example.com",
		"http://A.Example.COM./":        "example.com",
		"http://one.co.uk/":             "one.co.uk",
		"http://x.one.co.uk/":           "one.co.uk",
		"http://foo.github.io/":         "foo.github.io",
		"http://bar.github.io/":         "bar.github.io",
		"http://127.0.0.1:8080/":        "127.0.0.1",
		"http://localhost/":             "localhost",
		"about:blank":                   "about:blank",
		"data:text/html,hi":             UnknownDomain,
		"":                              UnknownDomain,
	} {
		if got := RegistrableDomain(in); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func sendTarget() *platform.TargetInfo {
	t := el(func(t *platform.TargetInfo) {
		t.Tag, t.InputType, t.Name, t.Actionable, t.InForm, t.FormMethod = "button", "submit", "Send", true, true, "post"
		t.FormAction, t.Content = "http://a.test/send", "msg=hello"
	})
	return &t
}

// startGranted starts a session on a.test with a domain grant for it.
func startGranted(t *testing.T, mod func(*Config)) (*Service, *fakeBrowser, *[]Event, string) {
	t.Helper()
	svc, fb, ev := newTestService(t, mod)
	st, err := svc.Start(ctx, "http://a.test/form", "g", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApproveDomain(st.SessionID, "a.test", 0); err != nil {
		t.Fatal(err)
	}
	return svc, fb, ev, st.SessionID
}

func clicks(fb *fakeBrowser) (n int) {
	for _, c := range fb.calls {
		if strings.HasPrefix(c, "click") || strings.HasPrefix(c, "key") {
			n++
		}
	}
	return
}

func pendingView(t *testing.T, svc *Service, id string) *CommitView {
	t.Helper()
	v := svc.Sessions(id)
	if len(v) != 1 || v[0].Commit == nil {
		t.Fatalf("no pending commit: %+v", v)
	}
	return v[0].Commit
}

// Acceptance: after a session-wide write approval, Send and Enter in a form
// still prompt; the approval card carries domain, action and a screenshot; deny
// blocks with a clear refusal.
func TestCommitAlwaysAsksDespiteGrantAndDenyBlocks(t *testing.T) {
	svc, fb, events, id := startGranted(t, nil)
	fb.target = sendTarget()

	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("Send after a grant must prompt: %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("a Send reached the browser without approval")
	}
	c := pendingView(t, svc, id)
	if c.Domain != "a.test" || !strings.Contains(c.Action, "Send") || !c.HasShot || c.Rect != nil && c.Rect.W < 0 {
		t.Fatalf("card needs domain, action and screenshot: %+v", c)
	}
	if png, _, ok := svc.CommitShot(id, c.ID); !ok || png == "" {
		t.Fatal("no screenshot for the card")
	}
	// While the user decides, nothing else is written.
	if _, err := svc.Type(ctx, id, "changed", "g"); !errors.Is(err, ErrCommitPending) {
		t.Fatalf("writes while a commit is pending: %v", err)
	}
	if err := svc.DenyCommit(id, c.ID); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Click(ctx, id, "#send", nil, nil, "g")
	if !errors.Is(err, ErrCommitDenied) || !strings.Contains(err.Error(), "commit_denied") {
		t.Fatalf("denied commit must be a clear refusal: %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("a denied action ran")
	}
	// Enter in a form field is a commit too.
	fb.target = &platform.TargetInfo{Resolved: true, Tag: "input", InputType: "text", Entry: true, InForm: true, FormMethod: "post", Content: "q=1"}
	if _, err := svc.Key(ctx, id, "Enter", "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("Enter in a form must prompt: %v", err)
	}
	var asked, answered int
	for _, e := range *events {
		if e.Action == "commit_prompt" && e.Fingerprint != "" && e.Domain == "a.test" {
			asked++
		}
		if e.Action == "deny_commit" && e.Class == "H" && e.Fingerprint != "" {
			answered++
		}
	}
	if asked != 2 || answered != 1 {
		t.Fatalf("prompts and answers must be audited: asked=%d answered=%d", asked, answered)
	}
}

// D5: approval covers exactly one action. The agent repeats the call; it runs
// once; a changed page or composer is asked again.
func TestApprovedCommitRunsOnceAndOnlyForSameFingerprint(t *testing.T) {
	svc, fb, events, id := startGranted(t, nil)
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	c := pendingView(t, svc, id)

	if err := svc.ApproveCommit(id, "not-the-id"); !errors.Is(err, ErrNoPendingCommit) {
		t.Fatalf("a stale card must not approve: %v", err)
	}
	if err := svc.ApproveCommit(id, c.ID); err != nil {
		t.Fatal(err)
	}
	if clicks(fb) != 0 {
		t.Fatal("approving alone must not act; the agent repeats the call")
	}
	// The content changed after approval: not the approved action.
	changed := *sendTarget()
	changed.Content = "msg=something else entirely"
	fb.target = &changed
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("a changed composer must ask again: %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("a changed action ran on the old approval")
	}
	// Restore and approve the new one, then it runs exactly once.
	if err := svc.DenyCommit(id, pendingView(t, svc, id).ID); err != nil {
		t.Fatal(err)
	}
	fb.target = sendTarget()
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); err != nil {
		t.Fatalf("the approved action must run: %v", err)
	}
	if clicks(fb) != 1 {
		t.Fatalf("clicks = %d", clicks(fb))
	}
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("an approval is single use: %v", err)
	}
	var ran bool
	for _, e := range *events {
		if e.Action == "click" && e.Outcome == "ok" && e.Fingerprint != "" && e.Label == "Send" {
			ran = true
		}
	}
	if !ran {
		t.Fatal("the executed commit must be audited with its fingerprint")
	}
}

func TestApprovedCommitExpires(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	svc, fb, _, id := startGranted(t, func(cfg *Config) { cfg.Now = c.now })
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	_ = svc.ApproveCommit(id, pendingView(t, svc, id).ID)
	c.add(ApprovedCommitTTL + time.Second)
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("a stale approval must not run: %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("stale approval ran")
	}
}

// D2: unknown targets, including an inspect failure, ask.
func TestUnknownTargetIsCommit(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	fb.target = &platform.TargetInfo{Resolved: false, Reason: "target is inside iframe"}
	if _, err := svc.Click(ctx, id, "", fptr(10), fptr(10), "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("unresolved target: %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("unknown target ran")
	}
}

func fptr(f float64) *float64 { return &f }

func TestTakingControlDropsPendingCommit(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	c := pendingView(t, svc, id)
	if err := svc.HumanTakeControl(id); err != nil {
		t.Fatal(err)
	}
	if err := svc.ApproveCommit(id, c.ID); !errors.Is(err, ErrNoPendingCommit) {
		t.Fatalf("a request must not survive the page changing under the user: %v", err)
	}
}

// D3: a grant on domain A does not cover B.
func TestGrantIsPerRegistrableDomain(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	if _, err := svc.Navigate(ctx, id, "http://b.test/", "g"); !errors.Is(err, ErrApprovalRequired) || !strings.Contains(err.Error(), "b.test") {
		t.Fatalf("navigate to B on A's grant: %v", err)
	}
	// Subdomains share the registrable domain.
	if _, err := svc.Navigate(ctx, id, "http://www.a.test/x", "g"); err != nil {
		t.Fatalf("same registrable domain: %v", err)
	}
	// A page on B (reached some other way) cannot be written on A's grant.
	fb.sessions[id].url = "http://b.test/"
	if _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("click on B with only A granted: %v", err)
	}
	v := svc.Sessions(id)[0]
	if v.AskedDomain != "b.test" || !v.ApprovalAsked {
		t.Fatalf("the card must name the domain asked about: %+v", v)
	}
	if err := svc.ApproveDomain(id, "b.test", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); err != nil {
		t.Fatalf("after approving B: %v", err)
	}
	// Approve with no domain follows the domain asked about.
	fb.sessions[id].url = "http://c.test/"
	_, _ = svc.Click(ctx, id, "#x", nil, nil, "g")
	_ = svc.Approve(id, 0)
	if _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); err != nil {
		t.Fatalf("Approve covers the asked domain: %v", err)
	}
}

func TestClickLinkToOtherDomainNeedsThatGrant(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	fb.target = &platform.TargetInfo{Resolved: true, Tag: "a", Name: "Partner", Href: "http://partner.test/", Actionable: true}
	if _, err := svc.Click(ctx, id, "a", nil, nil, "g"); !errors.Is(err, ErrApprovalRequired) || !strings.Contains(err.Error(), "partner.test") {
		t.Fatalf("cross-domain link: %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("link click ran without the destination's grant")
	}
}

// D4 with a fake clock.
func TestGrantLifetimes(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	mod := func(cfg *Config) { cfg.Now = c.now; cfg.IdleLifetime = 72 * time.Hour }
	write := func(svc *Service, id string) error {
		_, err := svc.Scroll(ctx, id, "", nil, nil, 0, 1, "g")
		return err
	}

	t.Run("not identity-bearing: 8h default, 24h max", func(t *testing.T) {
		svc, _, _ := newTestService(t, mod)
		st, _ := svc.Start(ctx, "http://a.test/", "g", "")
		id := st.SessionID
		_ = svc.ApproveDomain(id, "a.test", 0)
		c.add(7*time.Hour + 59*time.Minute)
		if err := write(svc, id); err != nil {
			t.Fatalf("inside 8h: %v", err)
		}
		c.add(2 * time.Minute)
		if err := write(svc, id); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("past 8h: %v", err)
		}
		_ = svc.ApproveDomain(id, "a.test", 100*time.Hour)
		c.add(23*time.Hour + 59*time.Minute)
		if err := write(svc, id); err != nil {
			t.Fatalf("inside the 24h maximum: %v", err)
		}
		c.add(2 * time.Minute)
		if err := write(svc, id); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("a request above 24h is capped at 24h: %v", err)
		}
	})

	t.Run("identity-bearing: 1h absolute", func(t *testing.T) {
		svc, _, _ := newTestService(t, mod)
		st, _ := svc.Start(ctx, "http://a.test/", "g", "")
		id := st.SessionID
		// The human drives the page (signs in), then hands back.
		if _, err := svc.Handoff(ctx, id, "log in", "g"); err != nil {
			t.Fatal(err)
		}
		if err := svc.HumanHandBack(id); err != nil {
			t.Fatal(err)
		}
		_ = svc.ApproveDomain(id, "a.test", 8*time.Hour)
		for i := 0; i < 5; i++ { // keep it busy so idle never trips
			c.add(11 * time.Minute)
			if err := write(svc, id); err != nil {
				t.Fatalf("minute %d: %v", (i+1)*11, err)
			}
		}
		c.add(6 * time.Minute) // 61 minutes in
		if err := write(svc, id); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("past the 1h absolute cap: %v", err)
		}
	})

	t.Run("identity-bearing: 15 minute idle", func(t *testing.T) {
		svc, _, _ := newTestService(t, mod)
		st, _ := svc.Start(ctx, "http://a.test/", "g", "")
		id := st.SessionID
		_, _ = svc.Handoff(ctx, id, "log in", "g")
		_ = svc.HumanHandBack(id)
		_ = svc.ApproveDomain(id, "a.test", time.Hour)
		c.add(14 * time.Minute)
		if err := write(svc, id); err != nil {
			t.Fatalf("under the idle limit: %v", err)
		}
		c.add(14 * time.Minute)
		if err := write(svc, id); err != nil {
			t.Fatalf("activity resets idle: %v", err)
		}
		c.add(16 * time.Minute)
		if err := write(svc, id); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("past 15 idle minutes: %v", err)
		}
	})

	t.Run("a grant given first shortens when the human then drives", func(t *testing.T) {
		svc, _, _ := newTestService(t, mod)
		st, _ := svc.Start(ctx, "http://a.test/", "g", "")
		id := st.SessionID
		_ = svc.ApproveDomain(id, "a.test", 8*time.Hour)
		_, _ = svc.Handoff(ctx, id, "log in", "g")
		_ = svc.HumanHandBack(id)
		c.add(16 * time.Minute)
		if err := write(svc, id); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("identity rules apply to an earlier grant too: %v", err)
		}
	})

	t.Run("other domains are not identity-bearing", func(t *testing.T) {
		svc, fb, _ := newTestService(t, mod)
		st, _ := svc.Start(ctx, "http://a.test/", "g", "")
		id := st.SessionID
		_, _ = svc.Handoff(ctx, id, "log in", "g")
		_ = svc.HumanHandBack(id)
		fb.sessions[id].url = "http://other.test/"
		_ = svc.ApproveDomain(id, "other.test", 0)
		c.add(2 * time.Hour)
		if err := write(svc, id); err != nil {
			t.Fatalf("an unrelated domain keeps the long grant: %v", err)
		}
	})

	t.Run("grants end when the session stops", func(t *testing.T) {
		svc, _, _ := newTestService(t, mod)
		st, _ := svc.Start(ctx, "http://a.test/", "g", "")
		_ = svc.ApproveDomain(st.SessionID, "a.test", 0)
		if err := svc.Stop(ctx, st.SessionID); err != nil {
			t.Fatal(err)
		}
		st2, _ := svc.Start(ctx, "http://a.test/", "g", "")
		if err := write(svc, st2.SessionID); !errors.Is(err, ErrApprovalRequired) {
			t.Fatalf("a new session must be asked again: %v", err)
		}
	})
}

func TestRevokeClearsEveryGrantAndApprovedCommit(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	_ = svc.ApproveCommit(id, pendingView(t, svc, id).ID)
	_ = svc.Revoke(id)
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrApprovalRevoked) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestWaitCommit(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	r, err := svc.Wait(ctx, id, "commit", 50*time.Millisecond)
	if err != nil || r.Satisfied || !r.StillWaiting {
		t.Fatalf("pending: %+v %v", r, err)
	}
	_ = svc.DenyCommit(id, pendingView(t, svc, id).ID)
	r, _ = svc.Wait(ctx, id, "commit", time.Second)
	if !r.Satisfied || r.State != "denied" {
		t.Fatalf("denied: %+v", r)
	}
}
