package localcobrowse

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// Commit actions (aceteam#13687 D1, D2, D5). A commit is anything that sends,
// posts, submits, pays or destroys. It is never covered by a session grant:
// every one asks the user, with the exact action shown, and the approval is
// good for that one action only.
//
// What this classifier is, stated so nobody repeats "it is gated": it is a
// deterministic heuristic over the target element. Labels lie, icon-only
// buttons rely on aria-label, and a single-page app can send on any event. The
// default for anything it cannot resolve is commit, which bounds false
// negatives at the cost of more prompts. It is not an effect-level guard.

// RuleVersion is recorded in the audit log so a later reader knows which rules
// classified an action. Bump it when the rules change.
const RuleVersion = "local-commit-1"

// Class is how an action is treated.
type Class string

const (
	// ClassFill changes only local page state and is covered by the domain grant.
	ClassFill Class = "fill"
	// ClassNavigate loads another page and is covered by the grant of the
	// destination's registrable domain.
	ClassNavigate Class = "navigate"
	// ClassCommit always asks.
	ClassCommit Class = "commit"
)

// Decision is Classify's answer.
type Decision struct {
	Class Class
	// Reason is a plain sentence for the approval card and the audit log. It
	// never contains page content beyond the element's own label.
	Reason string
	// Domain is the destination's registrable domain for ClassNavigate.
	Domain string
}

// Typed refusals for commit actions.
var (
	ErrCommitApprovalRequired = errors.New("commit_approval_required: this action sends, posts, submits, pays or deletes, so the user must approve it every time. They are being asked in the Citadel viewer, and if they approve it the action runs by itself. Do not repeat it; call local_cobrowse_wait with until=commit, then local_cobrowse_status or take a screenshot to see the result")
	ErrCommitPending          = errors.New("commit_pending: the user is deciding on an earlier send, post, submit or payment in this session. Do not change the page; call local_cobrowse_wait with until=commit")
	ErrCommitDenied           = errors.New("commit_denied: the user declined this action. Do not retry it; tell the user what you could not do and let them decide")
	ErrNoPendingCommit        = errors.New("that request is no longer current; look at the viewer for the latest one")
	ErrUnknownPage            = errors.New("unknown_page: this page's address cannot be tied to a website, so AI actions are refused here and cannot be approved; ask the user to take control or navigate somewhere else")
	ErrPageChanged            = errors.New("page_changed: the page changed while the action was being checked, so it was not sent; look at the page again and retry")
	ErrTypeNewline            = errors.New("type_newline: typed text may not contain a line break, which can send a message or submit a form; type the text, then use local_cobrowse_key with Enter, which asks the user")
)

// commitLabelWords are verbs that mark a control as a commit. Matching is on
// whole words (with a trailing s), case-insensitive. Extend this data, not the
// matcher.
var commitLabelWords = map[string]bool{
	"send": true, "post": true, "publish": true, "reply": true, "comment": true,
	"share": true, "tweet": true, "submit": true, "confirm": true, "pay": true,
	"buy": true, "purchase": true, "order": true, "checkout": true, "donate": true,
	"transfer": true, "delete": true, "remove": true, "archive": true,
	"unsubscribe": true, "subscribe": true, "connect": true, "follow": true,
	"endorse": true, "invite": true, "accept": true, "sign": true, "agree": true,
	"withdraw": true, "book": true, "reserve": true, "apply": true, "logout": true,
	"signout": true, "deactivate": true, "authorize": true, "allow": true, "approve": true,
	"merge": true, "repost": true, "retweet": true, "trash": true, "discard": true,
	"block": true, "report": true, "upload": true,
	// A few other languages. CJK is matched as substrings below.
	"envoyer": true, "enviar": true, "senden": true, "publier": true, "publicar": true,
	"acheter": true, "comprar": true, "kaufen": true, "supprimer": true, "eliminar": true,
	"löschen": true, "bezahlen": true, "payer": true, "pagar": true,
}

var commitLabelCJK = []string{"发送", "發送", "提交", "发布", "發布", "支付", "购买", "購買", "删除", "刪除", "送信", "投稿", "購入", "削除", "결제", "보내기", "삭제"}

// destructiveLinkWords are the verbs that make a plain link (a GET) a commit.
var destructiveLinkWords = map[string]bool{
	"delete": true, "remove": true, "logout": true, "signout": true, "unsubscribe": true,
	"confirm": true, "deactivate": true, "cancel": true, "accept": true, "agree": true,
}

var destructiveLinkPath = regexp.MustCompile(`(?i)/(log-?out|sign-?out|delete|unsubscribe|confirm|deactivate)(/|$|\?)`)

// urlIsDestructive reports an address whose path or query asks to delete, log
// out, unsubscribe or similar (a GET that acts), such as /logout or ?action=delete.
func urlIsDestructive(u *url.URL) bool {
	if destructiveLinkPath.MatchString(u.Path) {
		return true
	}
	for k, vs := range u.Query() {
		if hasWord(k, destructiveLinkWords) {
			return true
		}
		for _, v := range vs {
			if hasWord(v, destructiveLinkWords) {
				return true
			}
		}
	}
	return hasWord(u.Fragment, destructiveLinkWords)
}

// ClassifyNavigation decides a direct navigate: an address that itself acts is a
// commit, anything else is covered by the destination domain's grant.
func ClassifyNavigation(raw string) Decision {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Decision{Class: ClassCommit, Reason: "the address could not be read"}
	}
	if urlIsDestructive(u) {
		return Decision{Class: ClassCommit, Reason: "this address changes or ends something (log out, delete, unsubscribe or similar)"}
	}
	return Decision{Class: ClassNavigate, Domain: RegistrableDomain(raw)}
}

var wordSplit = regexp.MustCompile(`[^\p{L}\p{N}]+`)

func hasWord(name string, words map[string]bool) bool {
	low := strings.ToLower(name)
	toks := wordSplit.Split(low, -1)
	for i, tok := range toks {
		if tok == "" {
			continue
		}
		if words[tok] || words[strings.TrimSuffix(tok, "s")] {
			return true
		}
		// "log out" and "sign out" are two words.
		if (tok == "log" || tok == "sign") && i+1 < len(toks) && toks[i+1] == "out" {
			return true
		}
	}
	return false
}

func labelIsCommit(name string) bool {
	if hasWord(name, commitLabelWords) {
		return true
	}
	for _, c := range commitLabelCJK {
		if strings.Contains(name, c) {
			return true
		}
	}
	return false
}

// activationKey reports whether a key press can fire the focused control.
func isEnter(key string) bool { return key == "Enter" }
func isSpace(key string) bool { return key == " " }

// Classify maps an action and its resolved target to a class (D1, D2). kind is
// click, key, select, type, scroll or navigate; key is the key for kind=key.
func Classify(kind, key string, t platform.TargetInfo) Decision {
	switch kind {
	case "scroll", "navigate":
		return Decision{Class: ClassFill}
	}
	if !t.Resolved {
		why := "the target could not be identified"
		if t.Reason != "" {
			why += " (" + t.Reason + ")"
		}
		return Decision{Class: ClassCommit, Reason: why + ", so it is treated as a send or submit"}
	}
	switch kind {
	case "type":
		return Decision{Class: ClassFill}
	case "select":
		if t.AutoSubmit {
			return Decision{Class: ClassCommit, Reason: "choosing this option submits the form"}
		}
		return Decision{Class: ClassFill}
	case "key":
		switch {
		case isEnter(key):
			switch {
			case t.Entry && t.Search:
				return Decision{Class: ClassFill}
			case t.Actionable && !t.Entry && strings.TrimSpace(t.Name) != "":
				// A named control or link: judged by what it is.
				return activation(t)
			}
			// Everything else defaults to commit: any other field, anything in a
			// form, an unnamed control, or nothing focused (page shortcuts).
			return Decision{Class: ClassCommit, Reason: "Enter can send or submit here"}
		case isSpace(key):
			if t.Actionable && !t.Entry {
				return activation(t)
			}
			return Decision{Class: ClassFill}
		case key == "Delete" || key == "Backspace" || len([]rune(key)) == 1:
			// Outside a text field a letter, digit or Delete is a page shortcut
			// (archive, delete, send in some apps).
			if t.Entry {
				return Decision{Class: ClassFill}
			}
			return Decision{Class: ClassCommit, Reason: "this key can be a page shortcut that acts on something"}
		default:
			return Decision{Class: ClassFill}
		}
	case "click":
		if t.Entry && !t.Actionable {
			return Decision{Class: ClassFill}
		}
		return activation(t)
	}
	// Any kind this classifier does not know is a commit.
	return Decision{Class: ClassCommit, Reason: "unknown action"}
}

func activation(t platform.TargetInfo) Decision {
	label := t.Name
	var link *url.URL
	if t.Href != "" {
		u, err := url.Parse(t.Href)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return Decision{Class: ClassCommit, Reason: "this link does not open a normal web page"}
		}
		link = u
	}
	// The label is checked before the href: many sites build their Send and
	// Post "buttons" from anchors, and a link named Send is not a navigation.
	if labelIsCommit(label) || (link == nil && hasWord(label, map[string]bool{"cancel": true})) {
		return Decision{Class: ClassCommit, Reason: "the control " + quoteLabel(label) + " sends, posts, submits, pays or deletes"}
	}
	if link != nil {
		if hasWord(label, destructiveLinkWords) || urlIsDestructive(link) {
			return Decision{Class: ClassCommit, Reason: "this link " + quoteLabel(label) + " changes or ends something"}
		}
		if !sameDocument(link, t.PageURL) {
			return Decision{Class: ClassNavigate, Domain: RegistrableDomain(t.Href)}
		}
		// A fragment-only link runs page script, not a navigation: fall through
		// to the structure rules, where a nameless one is a commit.
	}
	submitType := t.InputType == "submit" || t.InputType == "image" ||
		(t.Tag == "button" && (t.InputType == "" || t.InputType == "submit"))
	if submitType && t.InForm {
		if t.Search {
			return Decision{Class: ClassFill}
		}
		return Decision{Class: ClassCommit, Reason: "this submits a form"}
	}
	if t.Actionable && strings.TrimSpace(label) == "" {
		return Decision{Class: ClassCommit, Reason: "this control has no label, so what it does cannot be told"}
	}
	return Decision{Class: ClassFill}
}

// sameDocument reports whether link points into the page it is on (a fragment
// or an empty href), which does not load another page.
func sameDocument(link *url.URL, page string) bool {
	p, err := url.Parse(page)
	if err != nil {
		return false
	}
	a, b := *link, *p
	a.Fragment, b.Fragment = "", ""
	a.RawFragment, b.RawFragment = "", ""
	return a.String() == b.String()
}

func quoteLabel(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "(no label)"
	}
	return "\"" + truncRunes(name, 60) + "\""
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func originPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.Path
}

type actionRef struct {
	Kind     string
	Key      string
	Selector string
	X, Y     *float64
	// Value and Label are the option a select would choose; URL is a navigate
	// target. Typed text is never part of an action reference.
	Value, Label string
	URL          string
}

// fingerprint binds an approval to one action on one page state (D5). It
// covers the request itself (kind, key, selector or rounded point), the page
// (origin and path, never the query), the element (tag, role, type, label,
// link and form targets) and a hash of the form or composer content, so the
// text the user looked at cannot be swapped after approval. It does not
// include the element's on-screen position: a selector click scrolls first,
// which would make an unchanged action look different.
func fingerprint(a actionRef, t platform.TargetInfo) string {
	round := func(p *float64) any {
		if p == nil {
			return nil
		}
		return int(*p/8) * 8
	}
	content := sha256.Sum256([]byte(t.Content))
	b, _ := json.Marshal(struct {
		V       int
		Kind    string
		Key     string
		Sel     string
		X, Y    any
		Page    string
		Tag     string
		Role    string
		Type    string
		Name    string
		Href    string
		FMethod string
		FAction string
		Content string
		Value   string
		Label   string
		URL     string
	}{1, a.Kind, a.Key, a.Selector, round(a.X), round(a.Y), originPath(t.PageURL), t.Tag, t.Role, t.InputType,
		t.Name, originPath(t.Href), t.FormMethod, originPath(t.FormAction), hex.EncodeToString(content[:]), a.Value, a.Label, a.URL})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PendingCommitTTL is how long an unanswered request waits.
const PendingCommitTTL = 10 * time.Minute

// CommitView is what the viewer shows for a pending request. The screenshot is
// served separately (CommitShot) so the polled state stays small.
type CommitView struct {
	ID          string `json:"id"`
	Domain      string `json:"domain"`
	Action      string `json:"action"`
	Label       string `json:"label,omitempty"`
	Reason      string `json:"reason"`
	RequestedAt string `json:"requested_at"`
	Rect        *Rect  `json:"rect,omitempty"`
	HasShot     bool   `json:"has_shot"`
}

// Rect is the target's box and the viewport it was measured in.
type Rect = platform.TargetRect

// CommitResult is what became of the last commit request: executed, changed
// (the page differed from what was approved, so nothing ran), denied, failed,
// expired or revoked.
type CommitResult struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	Action string `json:"action"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	At     string `json:"at"`
}

type pendingCommit struct {
	ref         actionRef
	id          string
	fp          string
	domain      string
	action      string
	label       string
	reason      string
	requestedAt time.Time
	shot        string
	rect        platform.TargetRect
	// targetID is the page the commit was recorded on ("" without page
	// tracking); an approval only executes on that page.
	targetID string
}

func describeAction(a actionRef, t platform.TargetInfo) string {
	switch a.Kind {
	case "key":
		if isEnter(a.Key) {
			return "Press Enter"
		}
		return "Press a key"
	case "select":
		if opt := strings.TrimSpace(a.Label); opt != "" {
			return "Choose " + quoteLabel(opt)
		} else if opt := strings.TrimSpace(a.Value); opt != "" {
			return "Choose " + quoteLabel(opt)
		}
		return "Choose an option"
	case "navigate":
		if u, err := url.Parse(a.URL); err == nil && u.Host != "" {
			return "Open " + quoteLabel(u.Host+u.Path)
		}
		return "Open an address"
	case "click":
		if n := strings.TrimSpace(t.Name); n != "" {
			return "Click " + quoteLabel(n)
		}
		return "Click"
	}
	return a.Kind
}
