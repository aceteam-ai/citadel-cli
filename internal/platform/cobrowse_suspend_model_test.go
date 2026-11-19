package platform

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var suspendTestNow = time.Date(2026, 11, 19, 18, 0, 0, 123, time.UTC)

func TestSuspendModelDefaultBounds(t *testing.T) {
	if maxSuspendManifestBytes != 262144 || maxSuspendTabs != 64 || maxSuspendURLBytes != 8192 ||
		maxSuspendTitleBytes != 2048 || maxSuspendedPerOwner != 5 || maxSuspendedNodeBytes != 8388608 ||
		throwawaySuspendTTL != 24*time.Hour || persistentSuspendTTL != 168*time.Hour || suspendTombstoneTTL != 168*time.Hour {
		t.Fatal("suspend bounds changed without an explicit contract update")
	}
}

func suspendTestLive() sessionRecord {
	return sessionRecord{Version: sessionRecordVersion, ID: "cb-0123456789abcdef", State: RecordLive,
		Generation: 7, OwnerUserID: "owner-a", Handoff: true, Tracking: true, StartedAt: "earlier",
		Profile: "/private/live/profile", DebugPort: 9911, BrowserID: "browser-id", BrowserPID: 20,
		BrowserStart: 30, XvfbPID: 40, XvfbStart: 50, Display: ":52", Browser: "chrome", OwnerPID: 60,
		OwnerStart: 70, BrowserScope: "live.scope", XvfbScope: "xvfb.scope", ContainerID: "container",
		ContainerStart: "start", ControlPort: 9912, Runner: "container", PendingEnd: &EndedSession{Reason: "update"}}
}

func suspendTestManifest() suspendManifest {
	return suspendManifest{Version: suspendManifestVersion, SessionID: "cb-0123456789abcdef", Generation: 7,
		Tabs: []suspendTab{{URL: "https://example.invalid/path?token=private-sentinel", Title: "Page",
			ScrollX: -2, ScrollY: 30.5, Active: true}, {URL: "about:blank"}}}
}

func suspendedTestRecord(t *testing.T, persistent bool) sessionRecord {
	t.Helper()
	live := suspendTestLive()
	if persistent {
		live.Persistent, live.ProfileName = true, "profile-a"
	}
	r, err := planSuspendedRecord(live, "owner-a", live.ProfileName, 2, 100, nil, suspendTestNow)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSuspendModelStateRefusals(t *testing.T) {
	for _, state := range []string{"launching", "resuming", "suspending", "pending_unlock", "suspended", "expired", "exited", "", "future"} {
		t.Run(state, func(t *testing.T) {
			err := checkSuspendState(state, false, true)
			if err == nil {
				t.Fatalf("accepted state %q", state)
			}
			if state == "pending_unlock" && !strings.Contains(err.Error(), "unlock or stop") {
				t.Fatal(err)
			}
			if (state == "launching" || state == "resuming" || state == "suspending") && !strings.Contains(err.Error(), "busy") {
				t.Fatal(err)
			}
		})
	}
	for _, state := range []string{"running", "attached"} {
		for _, human := range []bool{false, true} {
			for _, owner := range []bool{false, true} {
				if err := checkSuspendState(state, human, owner); (err != nil) != (human && !owner) {
					t.Fatalf("state %s human %v owner %v: %v", state, human, owner, err)
				}
			}
		}
	}
}

func TestSuspendModelManifestRoundTripAndIdentity(t *testing.T) {
	m := suspendTestManifest()
	data, err := encodeSuspendManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSuspendManifest(data, m.SessionID, m.Generation)
	if err != nil || len(got.Tabs) != 2 || got.Tabs[0] != m.Tabs[0] {
		t.Fatalf("round trip: %v", err)
	}
	for _, tc := range []struct {
		id         string
		generation uint64
	}{{"cb-aaaaaaaaaaaaaaaa", 7}, {m.SessionID, 8}, {m.SessionID, 0}} {
		if _, err := decodeSuspendManifest(data, tc.id, tc.generation); err == nil {
			t.Fatal("accepted foreign identity")
		}
	}
	if strings.Contains(fmt.Sprint(m), "private-sentinel") || strings.Contains(fmt.Sprintf("%#v", m), "private-sentinel") || strings.Contains(suspendManifestSummary(m), "private-sentinel") {
		t.Fatal("diagnostic summary exposed private manifest")
	}
}

func TestSuspendModelManifestBounds(t *testing.T) {
	cases := map[string]func(*suspendManifest){
		"version":     func(m *suspendManifest) { m.Version++ },
		"unsafe id":   func(m *suspendManifest) { m.SessionID = "../session" },
		"oversize id": func(m *suspendManifest) { m.SessionID = "cb-" + strings.Repeat("a", 65) },
		"generation":  func(m *suspendManifest) { m.Generation = 0 },
		"empty":       func(m *suspendManifest) { m.Tabs = nil },
		"tab count": func(m *suspendManifest) {
			for len(m.Tabs) <= maxSuspendTabs {
				m.Tabs = append(m.Tabs, suspendTab{URL: "about:blank"})
			}
		},
		"URL length": func(m *suspendManifest) {
			m.Tabs[0].URL = "https://example.invalid/" + strings.Repeat("a", maxSuspendURLBytes)
		},
		"URL encoding":     func(m *suspendManifest) { m.Tabs[0].URL = "https://example.invalid/\xff" },
		"URL parse":        func(m *suspendManifest) { m.Tabs[0].URL = "https://example.invalid/%private-sentinel" },
		"scheme":           func(m *suspendManifest) { m.Tabs[0].URL = "javascript:private-sentinel" },
		"missing URL":      func(m *suspendManifest) { m.Tabs[0].URL = "" },
		"title bound":      func(m *suspendManifest) { m.Tabs[0].Title = strings.Repeat("t", maxSuspendTitleBytes+1) },
		"title control":    func(m *suspendManifest) { m.Tabs[0].Title = "bad\ntitle" },
		"title encoding":   func(m *suspendManifest) { m.Tabs[0].Title = "\xff" },
		"NaN":              func(m *suspendManifest) { m.Tabs[0].ScrollX = math.NaN() },
		"infinity":         func(m *suspendManifest) { m.Tabs[0].ScrollY = math.Inf(1) },
		"coordinate bound": func(m *suspendManifest) { m.Tabs[0].ScrollY = math.MaxInt32 + 1 },
		"active missing":   func(m *suspendManifest) { m.Tabs[0].Active = false },
		"active duplicate": func(m *suspendManifest) { m.Tabs[1].Active = true },
		"aggregate bytes": func(m *suspendManifest) {
			m.Tabs = []suspendTab{{URL: "about:blank", Active: true}}
			for len(m.Tabs) < maxSuspendTabs {
				m.Tabs = append(m.Tabs, suspendTab{URL: "https://example.invalid/" + strings.Repeat("a", 7000)})
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := suspendTestManifest()
			mutate(&m)
			_, err := encodeSuspendManifest(m)
			if err == nil {
				t.Fatal("accepted malformed capture")
			}
			if strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("error leaked a URL")
			}
		})
	}
	m := suspendTestManifest()
	m.Tabs[0].URL = "https://a.invalid/" + strings.Repeat("a", maxSuspendURLBytes-len("https://a.invalid/"))
	m.Tabs[0].Title = strings.Repeat("t", maxSuspendTitleBytes)
	m.Tabs[0].ScrollX = math.MaxInt32
	for len(m.Tabs) < maxSuspendTabs {
		m.Tabs = append(m.Tabs, suspendTab{URL: "about:blank"})
	}
	if _, err := encodeSuspendManifest(m); err != nil {
		t.Fatalf("valid boundary refused: %v", err)
	}
}

func TestSuspendModelDecodeIsStrictAndBounded(t *testing.T) {
	m := suspendTestManifest()
	good, _ := encodeSuspendManifest(m)
	bad := [][]byte{nil, []byte("{"), append(append([]byte{}, good...), []byte(" {}")...),
		[]byte(strings.Repeat(" ", maxSuspendManifestBytes+1)), []byte("\xff"),
		[]byte(strings.Replace(string(good), `"version":1`, `"version":1,"cookie":"private-sentinel"`, 1))}
	for i, data := range bad {
		if _, err := decodeSuspendManifest(data, m.SessionID, m.Generation); err == nil || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatalf("bad input %d not safely refused", i)
		}
	}
	// The encoded byte bound admits exactly the boundary, not just smaller data.
	boundary := append(append([]byte{}, good...), bytesOf(' ', maxSuspendManifestBytes-len(good))...)
	if _, err := decodeSuspendManifest(boundary, m.SessionID, m.Generation); err != nil {
		t.Fatal(err)
	}
}

func bytesOf(c byte, n int) []byte { return []byte(strings.Repeat(string(c), n)) }

func TestSuspendModelRecordClearsLiveEffectsAndPersistsMetadataOnly(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		r := suspendedTestRecord(t, persistent)
		if r.ID != suspendTestLive().ID || r.Generation != 7 || !r.Handoff || !r.Tracking || r.State != RecordSuspended {
			t.Fatal("lost lifecycle identity")
		}
		// No PID, port, decrypted path, ownership scope or stale update intent may
		// survive into a record whose browser is expected to be gone.
		if r.Profile != "" || r.DebugPort != 0 || r.ControlPort != 0 || r.Display != "" || r.BrowserID != "" ||
			r.BrowserPID != 0 || r.XvfbPID != 0 || r.OwnerPID != 0 || r.OwnerStart != 0 || r.BrowserStart != 0 ||
			r.XvfbStart != 0 || r.OwnerUID != nil || r.BrowserScope != "" || r.XvfbScope != "" ||
			r.ContainerID != "" || r.ContainerStart != "" || r.Runner != "" || r.PendingEnd != nil {
			t.Fatal("live process fields retained")
		}
		wantTTL := throwawaySuspendTTL
		if persistent {
			wantTTL = persistentSuspendTTL
		}
		expiry, err := suspendExpiry(r)
		if err != nil || !expiry.Equal(suspendTestNow.Add(wantTTL)) {
			t.Fatalf("expiry: %v", err)
		}
		dir := filepath.Join(t.TempDir(), "registry")
		if err := saveRecord(dir, r); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadRecord(dir, r.ID)
		if err != nil || loaded.Suspension == nil || *loaded.Suspension != *r.Suspension {
			t.Fatal("durable metadata round trip failed")
		}
		data, _ := json.Marshal(loaded)
		if strings.Contains(string(data), "private-sentinel") || strings.Contains(string(data), "/private/live") {
			t.Fatal("registry exposed private capture")
		}
	}
}

func TestSuspendModelRecordRefusesUnknownOwnerAndInvalidBinding(t *testing.T) {
	cases := map[string]func(*sessionRecord){
		"legacy owner":    func(r *sessionRecord) { r.OwnerUserID = "" },
		"foreign owner":   func(r *sessionRecord) { r.OwnerUserID = "owner-b" },
		"version":         func(r *sessionRecord) { r.Version++ },
		"not live":        func(r *sessionRecord) { r.State = RecordSuspended },
		"zero generation": func(r *sessionRecord) { r.Generation = 0 },
		"invalid id":      func(r *sessionRecord) { r.ID = "../outside" },
		"profile missing": func(r *sessionRecord) { r.Persistent = true },
		"profile foreign": func(r *sessionRecord) { r.ProfileName = "profile-b" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := suspendTestLive()
			mutate(&r)
			if _, err := planSuspendedRecord(r, "owner-a", "", 2, 100, nil, suspendTestNow); err == nil {
				t.Fatal("binding accepted")
			}
		})
	}
	for _, owner := range []string{"", "owner b", strings.Repeat("a", 129), "\xff"} {
		r := suspendTestLive()
		r.OwnerUserID = owner
		if _, err := planSuspendedRecord(r, owner, "", 2, 100, nil, suspendTestNow); err == nil {
			t.Fatal("invalid owner accepted")
		}
	}
	for _, tc := range []struct {
		tabs  int
		bytes int64
	}{{0, 100}, {65, 100}, {2, 0}, {2, -1}, {2, maxSuspendManifestBytes + 4097}} {
		if _, err := planSuspendedRecord(suspendTestLive(), "owner-a", "", tc.tabs, tc.bytes, nil, suspendTestNow); err == nil {
			t.Fatal("invalid storage accounting accepted")
		}
	}
}

func TestSuspendModelCapacityCountsAllRetainedBytesAndOwnerSlots(t *testing.T) {
	var records []sessionRecord
	for i := 0; i < maxSuspendedPerOwner; i++ {
		r := suspendedTestRecord(t, false)
		r.ID = fmt.Sprintf("cb-%016x", i)
		records = append(records, r)
	}
	if _, err := planSuspendedRecord(suspendTestLive(), "owner-a", "", 2, 100, records[:4], suspendTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := planSuspendedRecord(suspendTestLive(), "owner-a", "", 2, 100, records, suspendTestNow); err == nil {
		t.Fatal("owner count cap removed")
	}
	for _, state := range []string{RecordSuspended, "suspending", "resuming"} {
		var foreign []sessionRecord
		for i := 0; i < 32; i++ {
			r := suspendedTestRecord(t, false)
			r.ID, r.State = fmt.Sprintf("cb-%016x", i), state
			r.OwnerUserID = fmt.Sprintf("foreign-owner-%d", i)
			r.Suspension.OwnerUserID = r.OwnerUserID
			r.Suspension.ManifestBytes = maxSuspendManifestBytes
			foreign = append(foreign, r)
		}
		foreign[31].Suspension.ManifestBytes -= 100
		if _, err := planSuspendedRecord(suspendTestLive(), "owner-a", "", 2, 100, foreign, suspendTestNow); err != nil {
			t.Fatal(err)
		}
		foreign[31].Suspension.ManifestBytes++
		if _, err := planSuspendedRecord(suspendTestLive(), "owner-a", "", 2, 100, foreign, suspendTestNow); err == nil {
			t.Fatal("node byte cap removed")
		}
		foreign[31].Suspension = nil
		if _, err := planSuspendedRecord(suspendTestLive(), "owner-a", "", 2, 100, foreign, suspendTestNow); err == nil {
			t.Fatal("unknown accounting accepted")
		}
	}
	r := suspendedTestRecord(t, false)
	if _, err := planSuspendedRecord(suspendTestLive(), "owner-a", "", 2, 100, []sessionRecord{r}, suspendTestNow); err == nil {
		t.Fatal("duplicate storage accepted")
	}
}

func TestSuspendModelReopenFencesAndExactExpiry(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		r := suspendedTestRecord(t, persistent)
		expiry, _ := suspendExpiry(r)
		got, err := planReopenRecord(r, "owner-a", true, expiry.Add(-time.Nanosecond))
		if err != nil || got.ID != r.ID || got.Generation != r.Generation+1 || got.State != "resuming" || !got.Handoff {
			t.Fatalf("reopen: %v", err)
		}
		if r.State != RecordSuspended || r.Generation != 7 {
			t.Fatal("plan mutated source record")
		}
		if _, err := planReopenRecord(r, "owner-a", false, suspendTestNow); (err != nil) != persistent {
			t.Fatal("human unlock boundary")
		}
		for _, now := range []time.Time{expiry, expiry.Add(time.Nanosecond), {}} {
			if _, err := planReopenRecord(r, "owner-a", true, now); err == nil {
				t.Fatal("reopened expired or unknown clock")
			}
		}
		if _, err := planReopenRecord(r, "owner-b", true, suspendTestNow); err == nil {
			t.Fatal("foreign owner reopened")
		}
		r.Generation = math.MaxUint64
		if _, err := planReopenRecord(r, "owner-a", true, suspendTestNow); err == nil {
			t.Fatal("generation wrapped")
		}
	}
}

func TestSuspendModelExpiryAndSevenDayTombstone(t *testing.T) {
	r := suspendedTestRecord(t, true)
	expiry, _ := suspendExpiry(r)
	if _, due, err := planSuspendExpiry(r, expiry.Add(-time.Nanosecond)); err != nil || due {
		t.Fatal("expired early")
	}
	expired, due, err := planSuspendExpiry(r, expiry)
	if err != nil || !due || expired.State != RecordExpired || expired.EndedReason != "suspend_expired" ||
		expired.Suspension.ManifestBytes != 0 || expired.Suspension.TabCount != 0 || expired.ProfileName != "profile-a" {
		t.Fatal("bad expiry plan")
	}
	if r.Suspension.TabCount != 2 || r.Suspension.ManifestBytes != 100 {
		t.Fatal("expiry aliased original metadata")
	}
	if _, again, err := planSuspendExpiry(expired, expiry.Add(time.Hour)); err != nil || again {
		t.Fatal("repeated expiry extended retention")
	}
	if suspendTombstoneDue(expired, expiry.Add(suspendTombstoneTTL-time.Nanosecond)) {
		t.Fatal("deleted tombstone early")
	}
	if !suspendTombstoneDue(expired, expiry.Add(suspendTombstoneTTL)) {
		t.Fatal("did not retire tombstone")
	}
	expired.EndedAt = "broken"
	if suspendTombstoneDue(expired, expiry.Add(100*suspendTombstoneTTL)) {
		t.Fatal("deleted malformed tombstone")
	}
}

func TestSuspendModelCorruptExpiryNeverReopensOrExpires(t *testing.T) {
	for _, mutate := range []func(*sessionRecord){
		func(r *sessionRecord) { r.Suspension = nil },
		func(r *sessionRecord) { r.Suspension.OwnerUserID = "foreign" },
		func(r *sessionRecord) { r.Suspension.ProfileName = "foreign" },
		func(r *sessionRecord) { r.Suspension.TabCount = 0 },
		func(r *sessionRecord) { r.Suspension.ManifestBytes = -1 },
		func(r *sessionRecord) { r.Suspension.SuspendedAt = "bad" },
		func(r *sessionRecord) {
			r.Suspension.ExpiresAt = suspendTestNow.Add(48 * time.Hour).Format(time.RFC3339Nano)
		},
	} {
		r := suspendedTestRecord(t, false)
		mutate(&r)
		if _, err := planReopenRecord(r, "owner-a", true, suspendTestNow); err == nil {
			t.Fatal("corrupt reopen accepted")
		}
		if _, due, err := planSuspendExpiry(r, suspendTestNow.Add(30*24*time.Hour)); err == nil || due {
			t.Fatal("corrupt expiry accepted")
		}
	}
}
