package workercontrol_test

import (
	"encoding/json"
	"strings"
	"testing"

	wc "github.com/aceteam-ai/citadel-cli/internal/workercontrol"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol/controltest"
)

func TestWorkerControlCanonicalVectors(t *testing.T) {
	f := controltest.Load()
	if wc.PayloadDigest(f.Payload) != f.Identity.PayloadDigest {
		t.Fatal("payload digest drift")
	}
	for name := range f.Cases {
		t.Run(name, func(t *testing.T) {
			r := f.Request(name)
			if r.Validate() != nil {
				t.Fatal("vector invalid")
			}
			copy := r
			copy.Seal()
			if copy != r {
				t.Fatal("cross-language digest mismatch")
			}
			raw, _ := json.Marshal(r)
			var decoded wc.Request
			if wc.Decode(raw, &decoded, 16384) != nil || decoded != r {
				t.Fatal("strict roundtrip")
			}
		})
	}
	a, b := f.Request("claimA"), f.Request("claimB")
	if a.OperationDigest != b.OperationDigest || a.RequestDigest == b.RequestDigest {
		t.Fatal("delivery entered semantic identity or lost request binding")
	}
	for _, name := range []string{"claimA", "completeA", "statusA"} {
		r := f.Request(name)
		s := controltest.Accept(r, 1800000001001)
		if name == "statusA" {
			p := f.Receipts["complete"]
			o := f.Observations["status"]
			s.Receipt = &p
			s.Observation = &o
		}
		if e := wc.ValidateResponse(r, s); e != nil {
			t.Fatalf("%s: %v", name, e)
		}
	}
}
func TestWorkerControlStrictRawBoundaries(t *testing.T) {
	f := controltest.Load()
	raw, _ := json.Marshal(f.Request("claimA"))
	base := string(raw)
	cases := map[string]string{
		"duplicate request": strings.Replace(base, `"request_id":`, `"request_id":"99999999-9999-4999-8999-999999999999","request_id":`, 1),
		"escaped duplicate": strings.Replace(base, `"binding_version":7`, `"binding_version":7,"binding_\u0076ersion":7`, 1),
		"unknown identity":  strings.Replace(base, `"binding_version":7`, `"binding_version":7,"unknown":true`, 1),
		"missing identity":  strings.Replace(base, `"binding_version":7,`, "", 1),
		"exponent":          strings.Replace(base, `"binding_version":7`, `"binding_version":7e0`, 1),
		"fraction":          strings.Replace(base, `"binding_version":7`, `"binding_version":7.0`, 1),
		"string integer":    strings.Replace(base, `"binding_version":7`, `"binding_version":"7"`, 1),
		"unsafe integer":    strings.Replace(base, `"binding_version":7`, `"binding_version":9007199254740992`, 1),
		"boolean integer":   strings.Replace(base, `"binding_version":7`, `"binding_version":true`, 1),
		"null identity":     strings.Replace(base, `"binding_version":7`, `"binding_version":null`, 1),
		"lone surrogate":    strings.Replace(base, `"claim_epoch":""`, `"claim_epoch":"\ud800"`, 1),
		"invalid utf8":      strings.Replace(base, `"claim_epoch":""`, "\"claim_epoch\":\"\xff\"", 1),
		"bom":               "\xef\xbb\xbf" + base,
		"trailing":          base + "{}",
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			var r wc.Request
			if wc.Decode([]byte(bad), &r, 16384) == nil {
				t.Fatal("ambiguous raw input accepted")
			}
		})
	}
	data := f.Data()
	d, _ := json.Marshal(data)
	if _, _, e := wc.ParseData(d, f.Deliveries["A"].MessageID, f.Identity.Queue, wc.Protocol); e != nil {
		t.Fatal(e)
	}
	data.Payload = `{"target_node":"758","action":"restart"}`
	d, _ = json.Marshal(data)
	if _, _, e := wc.ParseData(d, f.Deliveries["A"].MessageID, f.Identity.Queue, wc.Protocol); e != nil {
		t.Fatal("semantic key reordering changed payload digest")
	}
	data.Payload = `{"action":"restart","target_node":"758","target_node":"758"}`
	d, _ = json.Marshal(data)
	if _, _, e := wc.ParseData(d, f.Deliveries["A"].MessageID, "", ""); e == nil {
		t.Fatal("duplicate payload decoded before strict check")
	}
}
func TestWorkerControlResponseCorrelationAndHistory(t *testing.T) {
	f := controltest.Load()
	r := f.Request("completeA")
	edits := map[string]func(*wc.Response){
		"request":          func(s *wc.Response) { s.RequestID = f.Cases["claimA"].RequestID },
		"operation":        func(s *wc.Response) { s.Operation = "claim_and_ack" },
		"attempt":          func(s *wc.Response) { s.Identity.AttemptID = s.Identity.JobID },
		"binding":          func(s *wc.Response) { s.Identity.BindingVersion++ },
		"delivery":         func(s *wc.Response) { s.Delivery = f.Deliveries["B"] },
		"receipt delivery": func(s *wc.Response) { s.Receipt.Delivery = f.Deliveries["B"] },
		"epoch":            func(s *wc.Response) { s.Receipt.ClaimEpoch = s.Receipt.ReceiptID },
		"digest":           func(s *wc.Response) { s.Receipt.OutcomeDigest = strings.Repeat("0", 64) },
		"auth generation":  func(s *wc.Response) { s.Observation.AuthorizationGeneration++ },
		"rollback":         func(s *wc.Response) { s.Observation.StateVersion = 2 },
		"no receipt":       func(s *wc.Response) { s.Receipt = nil },
		"no observation":   func(s *wc.Response) { s.Observation = nil },
		"generic success":  func(s *wc.Response) { s.Decision = "published" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			s := controltest.Accept(r, 1800000001001)
			edit(&s)
			if wc.ValidateResponse(r, s) == nil {
				t.Fatal("uncorrelated permit accepted")
			}
		})
	}
	s := controltest.Accept(r, 1800000001001)
	s.Decision = "denied"
	s.Code = "refused"
	s.Receipt = nil
	s.Observation = nil
	if wc.ValidateResponse(r, s) != wc.ErrDenied {
		t.Fatal("explicit denial was transport success")
	}
	s.Decision = "unavailable"
	if wc.ValidateResponse(r, s) != wc.ErrUnavailable {
		t.Fatal("unavailable not typed")
	}
	// All fields, including explicit nullable fields, remain required.
	raw, _ := json.Marshal(s)
	var out wc.Response
	if wc.Decode(raw, &out, 16384) != nil {
		t.Fatal("required nulls rejected")
	}
	bad := strings.Replace(string(raw), `,"receipt":null`, "", 1)
	if wc.Decode([]byte(bad), &out, 16384) == nil {
		t.Fatal("omitted nullable field accepted")
	}
}
