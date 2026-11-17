package localcobrowse

import (
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func fileScenario(field, name string, size int, typ string, modified int) scenario {
	return scenario{Form: &fakeForm{
		Elements:  []fakeEl{fel("INPUT", "file", field, ""), button("go", "1", nil)},
		Submitted: [][]any{{field, map[string]any{"file": true, "name": name, "size": size, "type": typ, "lastModified": modified}}, {"go", "1"}},
	}, T: 1}
}

func TestFileSnapshotSensitiveCollisionAndBounds(t *testing.T) {
	collision := fileScenario("pw", "invoice.txt", 8, "text/plain", 100)
	collision.Form.Elements = []fakeEl{fel("INPUT", "password", "pw", "\x00file"), button("go", "1", nil)}
	if r := runInspect(t, collision)[0]; r.Bad == "" {
		t.Fatal("a File under a sensitive name must never be excluded as a secret string")
	}
	many := fileScenario("att", "invoice.txt", 8, "text/plain", 100)
	for i := 0; i < 128; i++ {
		many.Form.Submitted = append(many.Form.Submitted, many.Form.Submitted[0])
	}
	if r := runInspect(t, many)[0]; r.Bad == "" {
		t.Fatal("unbounded file card")
	}
}

func TestFileCommitCardCopiesMetadataWithoutAuditLeak(t *testing.T) {
	svc, fb, events, id := startGranted(t, nil)
	target := sendTarget()
	target.FormFiles = []platform.FormFile{{Field: "att", Name: "invoice\n.txt", Size: 8, Type: "text/plain", LastModified: 100}}
	fb.target = target
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatal(err)
	}
	card := pendingView(t, svc, id)
	if len(card.Files) != 1 || card.Files[0].Name != "invoice .txt" || card.Files[0].Size != 8 {
		t.Fatalf("file card: %+v", card)
	}
	card.Files[0].Name = "mutated"
	if pendingView(t, svc, id).Files[0].Name == "mutated" {
		t.Fatal("viewer mutated the pending approval")
	}
	for _, event := range *events {
		if strings.Contains(event.Label, "invoice") {
			t.Fatal("file metadata leaked into action label/audit")
		}
	}
}

func TestFileCommitMetadataIsPrivateWhileHumanDrives(t *testing.T) {
	svc, fb, _, id := startGranted(t, nil)
	target := sendTarget()
	target.FormFiles = []platform.FormFile{{Field: "att", Name: "human-private.txt", Size: 73, Type: "text/plain", LastModified: 91234}}
	fb.target = target
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatal(err)
	}
	// Model a browser-originated handoff before the service has expired its
	// old card. Even this pending evidence must not leak through an agent read.
	fb.mu.Lock()
	fb.sessions[id].human = true
	st := fb.statusLocked(id)
	fb.mu.Unlock()
	for _, requested := range []string{id, ""} {
		views, err := svc.Status(ctx, requested)
		if err != nil || len(views) != 1 || views[0].Commit == nil || len(views[0].Commit.Files) != 0 {
			t.Fatalf("agent status retained file evidence: %+v %v", views, err)
		}
	}
	svc.mu.Lock()
	view := svc.agentViewLocked(svc.sessions[id], st, svc.cfg.Now())
	svc.mu.Unlock()
	if view.Commit == nil || len(view.Commit.Files) != 0 {
		t.Fatalf("agent response retained file evidence: %+v", view)
	}
	card := pendingView(t, svc, id)
	if len(card.Files) != 1 || card.Files[0].Name != "human-private.txt" || card.Files[0].Size != 73 || card.Files[0].LastModified != 91234 {
		t.Fatalf("agent redaction damaged viewer evidence: %+v", card)
	}
}

func TestFileSnapshotEmptyAndMetadataBinding(t *testing.T) {
	empty := runInspect(t, fileScenario("att", "", 0, "application/octet-stream", 100), fileScenario("att", "", 0, "application/octet-stream", 200))
	if empty[0].Bad != "" || empty[0].Digest == "" || empty[0].Digest != empty[1].Digest {
		t.Fatalf("empty named input must be verifiable and stable: %+v", empty)
	}
	base := fileScenario("att", "invoice.txt", 8, "text/plain", 100)
	for name, changed := range map[string]scenario{
		"field":    fileScenario("other", "invoice.txt", 8, "text/plain", 100),
		"name":     fileScenario("att", "other.txt", 8, "text/plain", 100),
		"size":     fileScenario("att", "invoice.txt", 9, "text/plain", 100),
		"type":     fileScenario("att", "invoice.txt", 8, "application/pdf", 100),
		"modified": fileScenario("att", "invoice.txt", 8, "text/plain", 101),
	} {
		t.Run(name, func(t *testing.T) {
			r := runInspect(t, base, changed)
			if r[0].Bad != "" || r[1].Bad != "" || r[0].Digest == "" || r[0].Digest == r[1].Digest {
				t.Fatalf("file metadata must change the digest: %+v", r)
			}
			err, ran, state := redeem(t, targetFrom(r[0]), targetFrom(r[1]))
			if err != nil || ran || state != "changed" {
				t.Fatalf("changed file redeemed approval: err=%v ran=%v state=%s", err, ran, state)
			}
		})
	}
}

func TestFileInputClickIsFillButUploadButtonStillAsks(t *testing.T) {
	for _, typ := range []string{"file", "label"} {
		target := platform.TargetInfo{Resolved: true, Tag: "input", InputType: "file", FileChooser: true, Name: "Upload"}
		if typ == "label" {
			target.Tag = "label"
		}
		if got := Classify("click", "", target); got.Class != ClassFill {
			t.Fatalf("file input or its label: %+v", got)
		}
	}
	if got := Classify("click", "", platform.TargetInfo{Resolved: true, Tag: "button", Name: "Upload", Actionable: true}); got.Class != ClassCommit {
		t.Fatalf("upload button bypassed approval: %+v", got)
	}
}

func TestFileInputClickCannotBypassCommitSignals(t *testing.T) {
	for name, target := range map[string]platform.TargetInfo{
		"label attribute":       {Resolved: true, Tag: "label", InputType: "file", Name: "Submit"},
		"actionable file input": {Resolved: true, Tag: "input", InputType: "file", FileChooser: true, Actionable: true, Name: "Submit"},
		"actionable file label": {Resolved: true, Tag: "label", InputType: "file", FileChooser: true, Actionable: true, Name: "Upload"},
		"untrusted tag":         {Resolved: true, Tag: "span", InputType: "file", FileChooser: true, Name: "Submit"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Classify("click", "", target); got.Class != ClassCommit {
				t.Fatalf("commit signal bypassed: %+v", got)
			}
		})
	}
}

func TestFileSnapshotRejectsInvalidSize(t *testing.T) {
	for _, size := range []any{-1, 1.5, "8", nil, 9007199254740992.0} {
		s := fileScenario("att", "invoice.txt", 8, "text/plain", 100)
		s.Form.Submitted[0][1].(map[string]any)["size"] = size
		if got := runInspect(t, s)[0]; got.Bad == "" {
			t.Fatalf("invalid size %v was accepted", size)
		}
	}
}

func TestFileCommitCardNamesCannotHideExtension(t *testing.T) {
	for _, name := range []string{"invoice\u202efdp.exe", "invoice\u2066\u2069\u200e\u200f\u200b\u200d.exe", "invoice\u0085\u009f.exe", strings.Repeat("x", 120) + ".exe"} {
		got, ok := sanitizeFileName(name)
		if !ok || !strings.HasSuffix(got, ".exe") || len([]rune(got)) > 80 {
			t.Fatalf("unreadable filename: %q", got)
		}
		for _, bad := range []rune{'\u202e', '\u2066', '\u2069', '\u200e', '\u200f', '\u200b', '\u200d', '\u0085', '\u009f'} {
			if strings.ContainsRune(got, bad) || strings.ContainsRune(sanitizeLabel(name), bad) {
				t.Fatalf("format/control retained: %q", got)
			}
		}
	}
	if _, ok := sanitizeFileName("name." + strings.Repeat("x", 80)); ok {
		t.Fatal("unrepresentable extension was silently truncated")
	}
	svc, fb, _, id := startGranted(t, nil)
	target := sendTarget()
	target.FormFiles = []platform.FormFile{{Name: "name." + strings.Repeat("x", 80)}}
	fb.target = target
	if _, err := svc.Click(ctx, id, "#send", nil, nil, "g"); !errors.Is(err, ErrCommitApprovalRequired) {
		t.Fatal(err)
	}
	if err := svc.ApproveCommit(ctx, id, pendingView(t, svc, id).ID); err == nil || clicks(fb) != 0 {
		t.Fatalf("unreadable file card executed: %v", err)
	}
}
