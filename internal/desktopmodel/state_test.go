package desktopmodel

import (
	"bytes"
	"strings"
	"testing"
)

func fixtureState(t *testing.T) (validationState, manifest) {
	t.Helper()
	m := fixtureManifest()
	s, err := newState(strings.Repeat("a", 32), m)
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

func TestStateTransitions(t *testing.T) {
	s, m := fixtureState(t)
	if s.Status != "idle" || s.Sequence != 1 {
		t.Fatal("not initial")
	}
	checking, err := advanceState(s, "checking", m, nil, "")
	if err != nil || checking.Sequence != 2 || s.Status != "idle" {
		t.Fatal("checking transition mutated prior")
	}
	b := makeGzip(t, makeTar(t, []tarMember{{"fixture", []byte("benign"), 0}}))
	m = manifestFor(b)
	s, err = newState(strings.Repeat("a", 32), m)
	if err != nil {
		t.Fatal(err)
	}
	checking, err = advanceState(s, "checking", m, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := validateArchive(bytes.NewReader(b), m)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := advanceState(checking, "validated", m, receipt, "")
	if err != nil || validated.Sequence != 3 || validated.Status != "validated" {
		t.Fatal("verified receipt refused")
	}
	for _, terminal := range []validationState{validated, {1, s.OperationID, s.ManifestFingerprint, 3, "rejected", "archive_invalid"}} {
		if got, err := advanceState(terminal, "rejected", m, nil, "archive_invalid"); err != errState || got != (validationState{}) {
			t.Fatal("terminal rejection reset")
		}
		for _, next := range []string{"idle", "checking", "validated", "rejected"} {
			if got, err := advanceState(terminal, next, m, receipt, ""); err != errState || got != (validationState{}) {
				t.Fatal("terminal advanced")
			}
		}
	}
	for _, code := range []string{"archive_invalid", "storage_unavailable"} {
		got, err := advanceState(checking, "rejected", m, nil, code)
		if err != nil || got.ErrorCode != code || got.Sequence != 3 {
			t.Fatal("rejection transition")
		}
	}
	for name, bad := range map[string]*archiveReceipt{
		"nil": nil, "zero": {}, "fingerprint": {strings.Repeat("0", 64), receipt.archiveSHA, 1, 1},
		"sha": {receipt.fingerprint, strings.Repeat("0", 64), 1, 1}, "members": {receipt.fingerprint, receipt.archiveSHA, 0, 1}, "payload": {receipt.fingerprint, receipt.archiveSHA, 1, 0},
		"members_overflow": {receipt.fingerprint, receipt.archiveSHA, memberLimit + 1, 1},
		"payload_overflow": {receipt.fingerprint, receipt.archiveSHA, 1, payloadLimit + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := advanceState(checking, "validated", m, bad, ""); err != errState {
				t.Fatal("wrong receipt admitted")
			}
		})
	}
	wrong := m
	wrong.CompressedBytes++
	if _, err := advanceState(checking, "validated", wrong, receipt, ""); err != errState {
		t.Fatal("wrong descriptor admitted")
	}
	checking.Sequence = ^uint64(0)
	if _, err := advanceState(checking, "rejected", m, nil, "archive_invalid"); err != errState {
		t.Fatal("overflow admitted")
	}
	if _, err := newState("not an ID", m); err != errState {
		t.Fatal("invalid operation")
	}
	badManifest := m
	badManifest.SchemaVersion = 2
	if _, err := newState(strings.Repeat("a", 32), badManifest); err != errState {
		t.Fatal("invalid descriptor")
	}
}

func TestStateStrict(t *testing.T) {
	s, _ := fixtureState(t)
	b, err := s.canonical()
	if err != nil {
		t.Fatal(err)
	}
	base := string(b)
	if got, err := parseState(bytes.NewReader(b)); err != nil || got != s {
		t.Fatal("state roundtrip")
	}
	if strings.Contains(base, "ready") || strings.Contains(base, "path") || strings.Contains(base, "token") {
		t.Fatal("state claims runtime authority")
	}
	cases := map[string]string{
		"duplicate":    strings.Replace(base, `"sequence":1`, `"sequence":1,"sequence":1`, 1),
		"unknown":      strings.Replace(base, `"sequence":1`, `"sequence":1,"pid":1`, 1),
		"missing":      strings.Replace(base, `"sequence":1,`, "", 1),
		"zero":         strings.Replace(base, `"sequence":1`, `"sequence":0`, 1),
		"negative":     strings.Replace(base, `"sequence":1`, `"sequence":-1`, 1),
		"overflow":     strings.Replace(base, `"sequence":1`, `"sequence":18446744073709551616`, 1),
		"fraction":     strings.Replace(base, `"sequence":1`, `"sequence":1.0`, 1),
		"exponent":     strings.Replace(base, `"sequence":1`, `"sequence":1e0`, 1),
		"operation":    strings.Replace(base, strings.Repeat("a", 32), strings.Repeat("A", 32), 1),
		"fingerprint":  strings.Replace(base, s.ManifestFingerprint, "short", 1),
		"schema":       strings.Replace(base, `"schema_version":1`, `"schema_version":2`, 1),
		"runtime":      strings.Replace(base, `"status":"idle"`, `"status":"ready"`, 1),
		"raw_error":    strings.Replace(base, `"error_code":""`, `"error_code":"secret path"`, 1),
		"bad_rejected": strings.Replace(base, `"status":"idle"`, `"status":"rejected"`, 1),
		"null":         strings.Replace(base, `"status":"idle"`, `"status":null`, 1),
		"nested":       strings.Replace(base, `"operation_id":"`+s.OperationID+`"`, `"operation_id":{}`, 1),
		"trailing":     base + `{}`, "bom": "\xef\xbb\xbf" + base, "utf8": base + "\xff",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseState(strings.NewReader(input))
			if err != errState || got != (validationState{}) || err.Error() != "state_invalid" {
				t.Fatal("untrusted state admitted")
			}
		})
	}
	input := base + strings.Repeat(" ", stateLimit-len(base))
	if _, err := parseState(strings.NewReader(input)); err != nil {
		t.Fatal("exact cap")
	}
	if _, err := parseState(strings.NewReader(input + " ")); err != errState {
		t.Fatal("cap+1")
	}
	for _, next := range []validationState{
		{1, s.OperationID, s.ManifestFingerprint, 1, "checking", ""},
		{1, strings.Repeat("b", 32), s.ManifestFingerprint, 2, "checking", ""},
		{1, s.OperationID, strings.Repeat("b", 64), 2, "checking", ""},
		{1, s.OperationID, s.ManifestFingerprint, 2, "validated", ""},
	} {
		if lawfulPublication(s, next) {
			t.Fatal("unlawful publication")
		}
	}
}
