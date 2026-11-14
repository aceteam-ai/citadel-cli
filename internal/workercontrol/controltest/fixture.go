// Package controltest supplies inert cross-language vectors and synthetic server
// responses. It is not an implementation or proof of durable server authority.
package controltest

import (
	_ "embed"
	"encoding/json"

	"github.com/aceteam-ai/citadel-cli/internal/workercontrol"
)

//go:embed testdata/wire.json
var Raw []byte

type Case struct {
	Delivery        string `json:"delivery"`
	RequestID       string `json:"request_id"`
	Operation       string `json:"operation"`
	ClaimEpoch      string `json:"claim_epoch"`
	Outcome         string `json:"outcome"`
	Code            string `json:"code"`
	OutcomeDigest   string `json:"outcome_digest"`
	OperationDigest string `json:"operation_digest"`
	RequestDigest   string `json:"request_digest"`
}
type Fixture struct {
	Identity     workercontrol.Identity               `json:"identity"`
	Deliveries   map[string]workercontrol.Delivery    `json:"deliveries"`
	ProcessID    string                               `json:"process_id"`
	ClaimEpoch   string                               `json:"claim_epoch"`
	Payload      workercontrol.Payload                `json:"payload"`
	Cases        map[string]Case                      `json:"cases"`
	Receipts     map[string]workercontrol.Receipt     `json:"receipts"`
	Observations map[string]workercontrol.Observation `json:"observations"`
}

func Load() Fixture {
	var f Fixture
	if err := json.Unmarshal(Raw, &f); err != nil {
		panic(err)
	}
	return f
}
func (f Fixture) Request(name string) workercontrol.Request {
	c := f.Cases[name]
	return workercontrol.Request{RequestID: c.RequestID, Operation: c.Operation, Identity: f.Identity, Delivery: f.Deliveries[c.Delivery], ProcessID: f.ProcessID, ClaimEpoch: c.ClaimEpoch, Outcome: c.Outcome, Code: c.Code, OutcomeDigest: c.OutcomeDigest, OperationDigest: c.OperationDigest, RequestDigest: c.RequestDigest}
}
func (f Fixture) Config() workercontrol.Config {
	i := f.Identity
	return workercontrol.Config{Protocol: i.Protocol, Queue: i.Queue, Group: i.Group, BindingVersion: i.BindingVersion, AuthorizationGeneration: i.AuthorizationGeneration, ControlPlaneID: i.ControlPlaneID, APIKeyID: i.APIKeyID, OrgID: i.OrgID, TargetNode: i.TargetNode}
}
func (f Fixture) Data() workercontrol.Data {
	p, _ := json.Marshal(f.Payload)
	e, _ := json.Marshal(workercontrol.Envelope{Identity: f.Identity, Delivery: f.Deliveries["A"]})
	return workercontrol.Data{JobID: f.Identity.JobID, Type: "WORKER_CONTROL", Payload: string(p), WorkerControl: string(e)}
}
func Accept(r workercontrol.Request, now int64) workercontrol.Response {
	f := Load()
	p := f.Receipts["claim"]
	o := f.Observations["claim"]
	if r.Operation == "complete" {
		p = f.Receipts["complete"]
		o = f.Observations["complete"]
		p.ClaimEpoch = r.ClaimEpoch
		p.Outcome = r.Outcome
		p.Code = r.Code
		p.OutcomeDigest = r.OutcomeDigest
		if r.Outcome == "refused_no_effect" {
			p.State = "refused_no_effect"
			o.State = p.State
			o.PermitUsable = false
		}
	}
	p.OperationDigest = r.OperationDigest
	p.ProcessID = r.ProcessID
	p.Delivery = r.Delivery
	p.DecidedAtMS = now - 1
	o.ServerTimeMS = now
	o.BindingVersion = r.Identity.BindingVersion
	o.AuthorizationGeneration = r.Identity.AuthorizationGeneration
	return workercontrol.Response{RequestID: r.RequestID, Operation: r.Operation, Identity: r.Identity, Delivery: r.Delivery, ProcessID: r.ProcessID, OperationDigest: r.OperationDigest, RequestDigest: r.RequestDigest, Decision: "accepted", Code: "ok", Receipt: &p, Observation: &o}
}
