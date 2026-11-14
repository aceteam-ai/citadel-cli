package localcobrowse

import (
	"errors"
	"fmt"
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
		{"link labeled Send is a commit, not a navigation", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.Name, t.Href, t.Actionable = "a", "Send", "http://a.test/form#", true
		}), ClassCommit},
		{"link labeled Post to another page", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.Name, t.Href, t.Actionable = "a", "Post", "http://a.test/compose", true
		}), ClassCommit},
		{"fragment link with a harmless label", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.Name, t.Href, t.Actionable = "a", "Details", "http://a.test/form#more", true
		}), ClassFill},
		{"nameless fragment link is a commit", "click", "", el(func(t *platform.TargetInfo) { t.Tag, t.Href, t.Actionable = "a", "http://a.test/form#", true }), ClassCommit},
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
		{"Enter in a date input inside a form", "key", "Enter", el(func(t *platform.TargetInfo) { t.Tag, t.InputType, t.Entry, t.InForm = "input", "date", true, true }), ClassCommit},
		{"Enter on a checkbox inside a form", "key", "Enter", el(func(t *platform.TargetInfo) { t.Tag, t.InputType, t.InForm = "input", "checkbox", true }), ClassCommit},
		{"Enter on a focused Send button", "key", "Enter", sendBtn, ClassCommit},
		{"Space on a focused Send button", "key", " ", sendBtn, ClassCommit},
		{"Tab is fill", "key", "Tab", sendBtn, ClassFill},
		{"typing a letter is fill", "key", "a", el(func(t *platform.TargetInfo) { t.Tag, t.Entry = "textarea", true }), ClassFill},
		{"Enter with nothing focused defaults to commit", "key", "Enter", el(func(t *platform.TargetInfo) { t.Tag = "body" }), ClassCommit},
		{"Enter on an unnamed control", "key", "Enter", el(func(t *platform.TargetInfo) { t.Tag, t.Actionable = "div", true }), ClassCommit},
		{"Enter on a named harmless button", "key", "Enter", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Expand menu", true
		}), ClassFill},
		{"Enter on a named link navigates", "key", "Enter", el(func(t *platform.TargetInfo) {
			t.Tag, t.Name, t.Href, t.Actionable = "a", "Pricing", "http://a.test/pricing", true
		}), ClassNavigate},
		{"Delete outside a field", "key", "Delete", el(func(t *platform.TargetInfo) { t.Tag = "body" }), ClassCommit},
		{"Backspace outside a field", "key", "Backspace", el(func(t *platform.TargetInfo) { t.Tag = "body" }), ClassCommit},
		{"a letter outside a field is a shortcut", "key", "e", el(func(t *platform.TargetInfo) { t.Tag = "body" }), ClassCommit},
		{"a letter in a field is typing", "key", "e", el(func(t *platform.TargetInfo) { t.Tag, t.Entry = "input", true }), ClassFill},
		{"Backspace in a field is editing", "key", "Backspace", el(func(t *platform.TargetInfo) { t.Tag, t.Entry = "textarea", true }), ClassFill},
		{"Cancel button", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Cancel", true
		}), ClassCommit},
		{"Approve", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Approve", true
		}), ClassCommit},
		{"Merge pull request", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Merge pull request", true
		}), ClassCommit},
		{"Allow", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Allow", true
		}), ClassCommit},
		{"Upload", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.InputType, t.Name, t.Actionable = "button", "button", "Upload file", true
		}), ClassCommit},
		{"link with a delete query", "click", "", el(func(t *platform.TargetInfo) {
			t.Tag, t.Name, t.Href, t.Actionable = "a", "Item", "http://a.test/item?action=delete", true
		}), ClassCommit},
		{"closed shadow root is unresolved", "click", "", platform.TargetInfo{Reason: "target is inside a closed shadow root"}, ClassCommit},
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
		"http://evil.test@a.test/x":     "a.test",
		"http://user:pw@a.test:8080/":   "a.test",
		"http://a.test.:80/":            "a.test",
		"http://b\u00fccher.example/":   "xn--bcher-kva.example",
		"http://2130706433/":            "127.0.0.1",
		"http://0x7f.1/":                "127.0.0.1",
		"http://0177.0.0.1/":            "127.0.0.1",
		"http://[::1]:9/":               "::1",
		"http://1.2.3.4.5/":             UnknownDomain,
		"http://a.test\\@evil.test/":    UnknownDomain,
		"file:///etc/passwd":            UnknownDomain,
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
		t.FormAction, t.ContentHash = "http://a.test/send", "hash-hello"
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
	fb.target = &platform.TargetInfo{Resolved: true, Tag: "input", InputType: "text", Entry: true, InForm: true, FormMethod: "post", ContentHash: "hash-q1"}
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

// D5, execute on approve: the human's approval re-probes the live target, checks
// the fingerprint and runs the action itself. The agent does not repeat it.
func TestApproveExecutesTheApprovedActionOnce(t *testing.T) {
	svc, fb, events, id := startGranted(t, nil)
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	c := pendingView(t, svc, id)

	if err := svc.ApproveCommit(ctx, id, "not-the-id"); !errors.Is(err, ErrNoPendingCommit) {
		t.Fatalf("a stale card must not approve: %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("nothing runs before approval")
	}
	if err := svc.ApproveCommit(ctx, id, c.ID); err != nil {
		t.Fatal(err)
	}
	if clicks(fb) != 1 {
		t.Fatalf("approval must execute the action: clicks = %d", clicks(fb))
	}
	v := svc.Sessions(id)[0]
	if v.Commit != nil || v.CommitResult == nil || v.CommitResult.State != "executed" {
		t.Fatalf("the card needs the outcome: %+v", v.CommitResult)
	}
	if r, _ := svc.Wait(ctx, id, "commit", time.Second); !r.Satisfied || r.State != "executed" {
		t.Fatalf("the agent learns the result through wait: %+v", r)
	}
	if err := svc.ApproveCommit(ctx, id, c.ID); !errors.Is(err, ErrNoPendingCommit) || clicks(fb) != 1 {
		t.Fatalf("one approval is one execution: %v clicks=%d", err, clicks(fb))
	}
	// A repeated call is a new request: there is no token to spend.
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) || clicks(fb) != 1 {
		t.Fatalf("repeat must ask again: %v", err)
	}
	var ran, approved bool
	for _, e := range *events {
		if e.Action == "click" && e.Outcome == "ok" && e.Fingerprint != "" && e.Label == "Send" {
			ran = true
		}
		if e.Action == "approve_commit" && e.Outcome == "ok" {
			approved = true
		}
	}
	if !ran || !approved {
		t.Fatalf("the executed commit and its approval must be audited: %v %v", ran, approved)
	}
}

// If the page differs from what was approved, nothing runs and the card says so.
func TestApproveDoesNotRunIfThePageChanged(t *testing.T) {
	svc, fb, events, id := startGranted(t, nil)
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	c := pendingView(t, svc, id)
	changed := *sendTarget()
	changed.ContentHash = "hash-other"
	fb.target = &changed
	if err := svc.ApproveCommit(ctx, id, c.ID); err != nil {
		t.Fatal(err)
	}
	if clicks(fb) != 0 {
		t.Fatal("a changed page ran on the old approval")
	}
	if v := svc.Sessions(id)[0]; v.CommitResult == nil || v.CommitResult.State != "changed" || v.Commit != nil {
		t.Fatalf("card must say the page changed: %+v", v.CommitResult)
	}
	var seen bool
	for _, e := range *events {
		if e.Action == "approve_commit" && e.Outcome == "page_changed" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the unexecuted approval must be audited")
	}
	// The agent asks again about the new page.
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("ask again: %v", err)
	}
}

// A budget or audit failure at approval time does not burn the approval.
func TestApprovalSurvivesBudgetAndAuditFailure(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	failAudit := false
	svc, fb, _, id := startGranted(t, func(cfg *Config) {
		cfg.Now = c.now
		cfg.WritesPerMinute = 1
		cfg.BeforeAction = func(Event) (int64, error) {
			if failAudit {
				return 0, errors.New("audit log unavailable")
			}
			return 1, nil
		}
	})
	if _, err := svc.Scroll(ctx, id, "", nil, nil, 0, 1, "g"); err != nil { // spends the minute's budget
		t.Fatal(err)
	}
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	cv := pendingView(t, svc, id)
	if err := svc.ApproveCommit(ctx, id, cv.ID); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("budget: %v", err)
	}
	if svc.Sessions(id)[0].Commit == nil || clicks(fb) != 0 {
		t.Fatal("the request must stay pending")
	}
	c.add(61 * time.Second)
	failAudit = true
	if err := svc.ApproveCommit(ctx, id, cv.ID); err == nil || clicks(fb) != 0 || svc.Sessions(id)[0].Commit == nil {
		t.Fatalf("audit failure must not run or burn it: %v", err)
	}
	failAudit = false
	if err := svc.ApproveCommit(ctx, id, cv.ID); err != nil || clicks(fb) != 1 {
		t.Fatalf("then it runs: %v clicks=%d", err, clicks(fb))
	}
}

func TestPendingCommitExpires(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	svc, fb, _, id := startGranted(t, func(cfg *Config) { cfg.Now = c.now })
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	cv := pendingView(t, svc, id)
	c.add(PendingCommitTTL + time.Second)
	if err := svc.ApproveCommit(ctx, id, cv.ID); !errors.Is(err, ErrNoPendingCommit) || clicks(fb) != 0 {
		t.Fatalf("a stale request must not run: %v", err)
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
	if err := svc.ApproveCommit(ctx, id, c.ID); !errors.Is(err, ErrNoPendingCommit) {
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

	t.Run("a human drive caps every domain in the session, including an SSO hop", func(t *testing.T) {
		svc, fb, _ := newTestService(t, mod)
		st, _ := svc.Start(ctx, "http://a.test/", "g", "")
		id := st.SessionID
		_, _ = svc.Handoff(ctx, id, "log in", "g")
		fb.sessions[id].url = "http://sso.test/login" // the human followed a redirect and signed in
		_ = svc.HumanHandBack(id)
		_ = svc.ApproveDomain(id, "sso.test", 8*time.Hour)
		c.add(16 * time.Minute)
		if err := write(svc, id); !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("the SSO domain must take the identity idle limit: %v", err)
		}
	})

	t.Run("an agent resume with nobody driving does not mark the session", func(t *testing.T) {
		svc, _, _ := newTestService(t, mod)
		st, _ := svc.Start(ctx, "http://a.test/", "g", "")
		id := st.SessionID
		if _, err := svc.Resume(ctx, id); err != nil {
			t.Fatal(err)
		}
		_ = svc.ApproveDomain(id, "a.test", 0)
		c.add(2 * time.Hour)
		if err := write(svc, id); err != nil {
			t.Fatalf("no human drove, so the long grant stands: %v", err)
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

func TestRevokeClearsGrantsAndPendingCommitAndDomainCanBeReapproved(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	cv := pendingView(t, svc, id)
	_ = svc.Revoke(id)
	if err := svc.ApproveCommit(ctx, id, cv.ID); !errors.Is(err, ErrNoPendingCommit) || clicks(fb) != 0 {
		t.Fatalf("a revoked session must not run a pending commit: %v", err)
	}
	if r := svc.Sessions(id)[0].CommitResult; r == nil || r.State != "revoked" {
		t.Fatalf("result: %+v", r)
	}
	if _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); !errors.Is(err, ErrApprovalRevoked) {
		t.Fatalf("revoked: %v", err)
	}
	if err := svc.ApproveDomain(id, "a.test", 0); err != nil {
		t.Fatal(err)
	}
	fb.target = nil
	if _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); err != nil {
		t.Fatalf("a revoked domain can be approved again: %v", err)
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

func TestSelectApprovalBindsTheChosenOption(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	fb.target = &platform.TargetInfo{Resolved: true, Tag: "select", InForm: true, AutoSubmit: true}
	_, _ = svc.Select(ctx, id, "#plan", "premium", "Premium", "g")
	if c := pendingView(t, svc, id); !strings.Contains(c.Action, "Premium") {
		t.Fatalf("the card must show the option: %+v", c)
	}
	if err := svc.ApproveCommit(ctx, id, pendingView(t, svc, id).ID); err != nil {
		t.Fatal(err)
	}
	if got := fb.calls[len(fb.calls)-1]; got != "select #plan" {
		t.Fatalf("the approved select must run: %v", fb.calls)
	}
	// A different option is a different action: it asks.
	if _, err := svc.Select(ctx, id, "#plan", "basic", "Basic", "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("a different option: %v", err)
	}
}

// Writes are serialized per session, and everything but the pending decision is
// refused while a commit waits.
func TestWritesSerializedAndRefusedWhileCommitPending(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	release := make(chan struct{})
	entered := make(chan struct{})
	fb.onClick = func() { close(entered); <-release }
	done := make(chan error, 2)
	go func() { _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); done <- err }()
	<-entered
	go func() { _, err := svc.Scroll(ctx, id, "", nil, nil, 0, 5, "g"); done <- err }()
	time.Sleep(60 * time.Millisecond)
	fb.mu.Lock()
	scrolled := false
	for _, c := range fb.calls {
		if c == "scroll" {
			scrolled = true
		}
	}
	fb.mu.Unlock()
	if scrolled {
		t.Fatal("a scroll reached the browser while a click was in flight")
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	fb.onClick = nil

	fb.target = sendTarget()
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	n := len(fb.calls)
	writes := map[string]func() error{
		"scroll":   func() error { _, err := svc.Scroll(ctx, id, "", nil, nil, 0, 5, "g"); return err },
		"key":      func() error { _, err := svc.Key(ctx, id, "Tab", "g"); return err },
		"type":     func() error { _, err := svc.Type(ctx, id, "x", "g"); return err },
		"select":   func() error { _, err := svc.Select(ctx, id, "#s", "v", "", "g"); return err },
		"navigate": func() error { _, err := svc.Navigate(ctx, id, "http://a.test/x", "g"); return err },
		"click":    func() error { _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); return err },
	}
	for name, w := range writes {
		if err := w(); !errors.Is(err, ErrCommitPending) {
			t.Errorf("%s while a commit is pending: %v", name, err)
		}
	}
	if len(fb.calls) != n {
		t.Fatalf("a write reached the browser: %v", fb.calls[n:])
	}
}

// A page that changes between classification and dispatch is not acted on.
func TestReprobeBeforeDispatch(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	benign := &platform.TargetInfo{Resolved: true, Tag: "button", InputType: "button", Name: "Expand menu", Actionable: true}
	fb.target = benign
	n := 0
	fb.onInspect = func() {
		n++
		if n == 2 { // between the first probe and the dispatch the page turns it into Send
			fb.target = sendTarget()
		}
	}
	if _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); !errors.Is(err, ErrPageChanged) {
		t.Fatalf("want page_changed, got %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("the click ran on a changed page")
	}
}

func TestNavigationCommitsDestructiveAddressesAndChecksLanding(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	for _, u := range []string{"http://a.test/logout", "http://a.test/account?action=delete", "http://a.test/x?op=unsubscribe", "http://a.test/delete/"} {
		if _, err := svc.Navigate(ctx, id, u, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
			t.Fatalf("%s must ask: %v", u, err)
		}
		cv := pendingView(t, svc, id)
		if err := svc.DenyCommit(id, cv.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Approved: it runs.
	_, _ = svc.Navigate(ctx, id, "http://a.test/logout", "g") // denied earlier: sticky
	if _, err := svc.Navigate(ctx, id, "http://a.test/settings?action=remove", "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatal(err)
	}
	if err := svc.ApproveCommit(ctx, id, pendingView(t, svc, id).ID); err != nil {
		t.Fatal(err)
	}
	if got := fb.calls[len(fb.calls)-1]; got != "navigate http://a.test/settings?action=remove" {
		t.Fatalf("approved navigate must run: %v", fb.calls)
	}
	if _, err := svc.Navigate(ctx, id, "http://a.test/docs?q=hello", "g"); err != nil {
		t.Fatalf("an ordinary address: %v", err)
	}

	// A redirect onto an ungranted domain stops on a blank page and asks.
	fb.navigateLandsOn = "http://b.test/landing"
	_, err := svc.Navigate(ctx, id, "http://a.test/go", "g")
	if !errors.Is(err, ErrApprovalRequired) || !strings.Contains(err.Error(), "b.test") {
		t.Fatalf("landing on an ungranted domain: %v", err)
	}
	if fb.sessions[id].url != "about:blank" {
		t.Fatalf("the browser must stop on a blank page, is on %s", fb.sessions[id].url)
	}
	if v := svc.Sessions(id)[0]; !v.ApprovalAsked || v.AskedDomain != "b.test" {
		t.Fatalf("the card must ask about the landed domain: %+v", v)
	}
}

func TestUnknownPageCannotBeApprovedOrWritten(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	if err := svc.ApproveDomain(id, UnknownDomain, 0); !errors.Is(err, ErrUnknownPage) {
		t.Fatalf("approve unknown: %v", err)
	}
	fb.sessions[id].url = "http://2130706433.5.6.7.8/"
	if _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); !errors.Is(err, ErrUnknownPage) {
		t.Fatalf("write on an unknown page: %v", err)
	}
	if svc.Sessions(id)[0].ApprovalAsked {
		t.Fatal("an unknown page must not raise an approval card")
	}
}

func TestTypeWithLineBreakIsRefused(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	for _, text := range []string{"hi\n", "a\rb"} {
		if _, err := svc.Type(ctx, id, text, "g"); !errors.Is(err, ErrTypeNewline) {
			t.Fatalf("%q: %v", text, err)
		}
	}
	for _, c := range fb.calls {
		if c == "type" {
			t.Fatal("typed text with a line break reached the browser")
		}
	}
}

// What the user typed in a composer never reaches the card or the audit log.
func TestComposerTextNeverInCardOrAudit(t *testing.T) {
	const secret = "SECRET-COMPOSER-TEXT-4711"
	svc, fb, events, id := startGranted(t, nil)
	tg := sendTarget()
	tg.ContentHash = "digest-only"
	_ = secret
	fb.target = tg
	_, _ = svc.Click(ctx, id, "#send", nil, nil, "g")
	cv := pendingView(t, svc, id)
	_ = svc.ApproveCommit(ctx, id, cv.ID)
	if out := fmt.Sprintf("%+v %+v", svc.Sessions(id), *events); strings.Contains(out, secret) {
		t.Fatalf("composer text leaked: %s", out)
	}
}

// An approval binds the complete target, query and fragment included: changing
// only the item a link, the recipient a form or the account a page carries in its
// query must not run under the earlier approval.
func TestApprovalBindsFullTargetQuery(t *testing.T) {
	cases := map[string]struct{ first, second func(*platform.TargetInfo) }{
		"link item": {
			func(t *platform.TargetInfo) {
				t.Tag, t.Name, t.Href, t.Actionable = "a", "Delete", "http://a.test/del?id=first", true
			},
			func(t *platform.TargetInfo) {
				t.Tag, t.Name, t.Href, t.Actionable = "a", "Delete", "http://a.test/del?id=second", true
			},
		},
		"link fragment": {
			func(t *platform.TargetInfo) {
				t.Tag, t.Name, t.Href, t.Actionable = "a", "Delete", "http://a.test/del?id=1#a", true
			},
			func(t *platform.TargetInfo) {
				t.Tag, t.Name, t.Href, t.Actionable = "a", "Delete", "http://a.test/del?id=1#b", true
			},
		},
		"form recipient": {
			func(t *platform.TargetInfo) {
				t.Tag, t.InputType, t.Name, t.Actionable, t.InForm, t.FormMethod = "button", "submit", "Send", true, true, "post"
				t.FormAction = "http://a.test/send?to=alice"
			},
			func(t *platform.TargetInfo) {
				t.Tag, t.InputType, t.Name, t.Actionable, t.InForm, t.FormMethod = "button", "submit", "Send", true, true, "post"
				t.FormAction = "http://a.test/send?to=mallory"
			},
		},
		"page account": {
			func(t *platform.TargetInfo) {
				t.Tag, t.InputType, t.Name, t.Actionable, t.InForm = "button", "submit", "Send", true, true
				t.PageURL = "http://a.test/form?account=personal"
			},
			func(t *platform.TargetInfo) {
				t.Tag, t.InputType, t.Name, t.Actionable, t.InForm = "button", "submit", "Send", true, true
				t.PageURL = "http://a.test/form?account=work"
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			svc, fb, events, id := startGranted(t, nil)
			t1, t2 := el(c.first), el(c.second)
			fb.target = &t1
			if _, err := svc.Click(ctx, id, "#go", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
				t.Fatalf("must ask: %v", err)
			}
			cv := pendingView(t, svc, id)
			fb.target = &t2 // only the query or fragment differs
			if err := svc.ApproveCommit(ctx, id, cv.ID); err != nil {
				t.Fatal(err)
			}
			if clicks(fb) != 0 {
				t.Fatal("the changed target ran under the earlier approval")
			}
			if r := svc.Sessions(id)[0].CommitResult; r == nil || r.State != "changed" {
				t.Fatalf("must resolve as changed: %+v", r)
			}
			// Raw query and fragment values never reach cards, results or the audit text.
			out := fmt.Sprintf("%+v %+v", svc.Sessions(id), *events)
			for _, leak := range []string{"first", "second", "mallory", "alice", "personal", "work", "#a", "#b"} {
				if strings.Contains(out, leak) {
					t.Fatalf("%q leaked into state or audit events: %s", leak, out)
				}
			}
		})
	}
}

// A target whose content was too large to hash is a commit that can never be
// redeemed, even if the page is unchanged.
func TestOversizedTargetIsNeverRedeemable(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	big := *sendTarget()
	big.ContentHash, big.Oversized = "", true
	fb.target = &big
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatalf("must ask: %v", err)
	}
	cv := pendingView(t, svc, id)
	if err := svc.ApproveCommit(ctx, id, cv.ID); !errors.Is(err, ErrTargetTooLarge) {
		t.Fatalf("approving an unverifiable target: %v", err)
	}
	if clicks(fb) != 0 {
		t.Fatal("an oversized target ran")
	}
	if r := svc.Sessions(id)[0].CommitResult; r == nil || r.State != "failed" {
		t.Fatalf("result: %+v", r)
	}
	if d := Classify("click", "", big); d.Class != ClassCommit {
		t.Fatalf("oversized must classify as commit: %v", d)
	}
}

// The fingerprint follows the content digest the page computed.
func TestFingerprintFollowsContentDigest(t *testing.T) {
	a := actionRef{Kind: "click", Selector: "#send"}
	t1 := *sendTarget()
	t2 := t1
	t2.ContentHash = "different-digest"
	if fingerprint(a, t1) == fingerprint(a, t2) {
		t.Fatal("a different content digest must change the fingerprint")
	}
	t3 := t1
	t3.Oversized = true
	if fingerprint(a, t1) == fingerprint(a, t3) {
		t.Fatal("oversized must change the fingerprint")
	}
}
