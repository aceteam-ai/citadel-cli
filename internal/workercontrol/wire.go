// Package workercontrol owns the bounded worker-control-v2 wire contract.
// Transport delivery is never an execution permit.
package workercontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const Protocol = "worker-control-v2"
const MaxInteger int64 = 9007199254740991

var ErrInvalid = errors.New("invalid worker control wire contract")
var ErrDenied = errors.New("worker control denied")
var ErrUnavailable = errors.New("worker control authority unavailable")

type Identity struct {
	Protocol                string `json:"protocol"`
	OrgID                   string `json:"org_id"`
	ControlPlaneID          string `json:"control_plane_id"`
	TargetNode              string `json:"target_node"`
	APIKeyID                string `json:"api_key_id"`
	BindingVersion          int64  `json:"binding_version"`
	AuthorizationGeneration int64  `json:"authorization_generation"`
	AttemptID               string `json:"attempt_id"`
	JobID                   string `json:"jobId"`
	Queue                   string `json:"queue"`
	Group                   string `json:"group"`
	ExpiresAt               int64  `json:"expires_at"`
	PayloadDigest           string `json:"payload_digest"`
}
type Delivery struct {
	AssociationID string `json:"association_id"`
	Queue         string `json:"queue"`
	Group         string `json:"group"`
	MessageID     string `json:"message_id"`
}
type Envelope struct {
	Identity Identity `json:"identity"`
	Delivery Delivery `json:"delivery"`
}
type Payload struct {
	Action     string `json:"action"`
	TargetNode string `json:"target_node"`
}
type Config struct {
	Protocol                string `json:"protocol"`
	Queue                   string `json:"queue"`
	Group                   string `json:"group"`
	BindingVersion          int64  `json:"binding_version"`
	AuthorizationGeneration int64  `json:"authorization_generation"`
	ControlPlaneID          string `json:"control_plane_id"`
	APIKeyID                string `json:"api_key_id"`
	OrgID                   string `json:"org_id"`
	TargetNode              string `json:"target_node"`
}
type Request struct {
	RequestID       string   `json:"request_id"`
	Operation       string   `json:"operation"`
	Identity        Identity `json:"identity"`
	Delivery        Delivery `json:"delivery"`
	ProcessID       string   `json:"process_id"`
	ClaimEpoch      string   `json:"claim_epoch"`
	Outcome         string   `json:"outcome"`
	Code            string   `json:"code"`
	OutcomeDigest   string   `json:"outcome_digest"`
	OperationDigest string   `json:"operation_digest"`
	RequestDigest   string   `json:"request_digest"`
}
type Receipt struct {
	ReceiptID       string   `json:"receipt_id"`
	Operation       string   `json:"operation"`
	OperationDigest string   `json:"operation_digest"`
	ProcessID       string   `json:"process_id"`
	ClaimEpoch      string   `json:"claim_epoch"`
	Delivery        Delivery `json:"delivery"`
	Outcome         string   `json:"outcome"`
	Code            string   `json:"code"`
	OutcomeDigest   string   `json:"outcome_digest"`
	State           string   `json:"state"`
	StateVersion    int64    `json:"state_version"`
	DecidedAtMS     int64    `json:"decided_at_ms"`
}
type Observation struct {
	ServerTimeMS            int64  `json:"server_time_ms"`
	State                   string `json:"state"`
	StateVersion            int64  `json:"state_version"`
	BindingVersion          int64  `json:"binding_version"`
	AuthorizationGeneration int64  `json:"authorization_generation"`
	PermitUsable            bool   `json:"permit_usable"`
}
type Response struct {
	RequestID       string       `json:"request_id"`
	Operation       string       `json:"operation"`
	Identity        Identity     `json:"identity"`
	Delivery        Delivery     `json:"delivery"`
	ProcessID       string       `json:"process_id"`
	OperationDigest string       `json:"operation_digest"`
	RequestDigest   string       `json:"request_digest"`
	Decision        string       `json:"decision"`
	Code            string       `json:"code"`
	Receipt         *Receipt     `json:"receipt"`
	Observation     *Observation `json:"observation"`
}

var uuidText = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var hashText = regexp.MustCompile(`^[0-9a-f]{64}$`)
var codeText = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
var positiveText = regexp.MustCompile(`^[1-9][0-9]*$`)
var componentText = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

func UUID(s string) bool       { return uuidText.MatchString(s) }
func RandomUUID(s string) bool { return UUID(s) && s[14] == '4' }
func integer(n int64) bool     { return n > 0 && n <= MaxInteger }
func node(s string) bool {
	if !positiveText.MatchString(s) {
		return false
	}
	_, e := strconv.ParseUint(s, 10, 64)
	return e == nil
}
func message(s string) bool {
	p := strings.Split(s, "-")
	if len(p) != 2 || !componentText.MatchString(p[0]) || !componentText.MatchString(p[1]) || (p[0] == "0" && p[1] == "0") {
		return false
	}
	_, a := strconv.ParseUint(p[0], 10, 64)
	_, b := strconv.ParseUint(p[1], 10, 64)
	return a == nil && b == nil
}
func Queue(org, node string) string { return "jobs:v2:control:org_" + org + ":node:" + node }
func IsQueue(q string) bool {
	return strings.HasPrefix(q, "jobs:v1:control:") || strings.HasPrefix(q, "jobs:v2:control:")
}
func (i Identity) Validate() error {
	if i.Protocol != Protocol || i.Group != Protocol || !UUID(i.OrgID) || !UUID(i.ControlPlaneID) || !UUID(i.APIKeyID) || !RandomUUID(i.AttemptID) || !UUID(i.JobID) || !node(i.TargetNode) || !integer(i.BindingVersion) || !integer(i.AuthorizationGeneration) || !integer(i.ExpiresAt) || !hashText.MatchString(i.PayloadDigest) || i.Queue != Queue(i.OrgID, i.TargetNode) || len(i.Queue) > 256 {
		return ErrInvalid
	}
	return nil
}
func (d Delivery) Validate(i Identity) error {
	if !RandomUUID(d.AssociationID) || d.Queue != i.Queue || d.Group != i.Group || !message(d.MessageID) {
		return ErrInvalid
	}
	return nil
}
func (c Config) Matches(i Identity) bool {
	return c.Protocol == i.Protocol && c.Queue == i.Queue && c.Group == i.Group && c.BindingVersion == i.BindingVersion && c.AuthorizationGeneration == i.AuthorizationGeneration && c.ControlPlaneID == i.ControlPlaneID && c.APIKeyID == i.APIKeyID && c.OrgID == i.OrgID && c.TargetNode == i.TargetNode
}
func (c Config) Validate() error {
	i := Identity{Protocol: c.Protocol, Queue: c.Queue, Group: c.Group, BindingVersion: c.BindingVersion, AuthorizationGeneration: c.AuthorizationGeneration, ControlPlaneID: c.ControlPlaneID, APIKeyID: c.APIKeyID, OrgID: c.OrgID, TargetNode: c.TargetNode, AttemptID: "44444444-4444-4444-8444-444444444444", JobID: "33333333-3333-4333-8333-333333333333", ExpiresAt: 1, PayloadDigest: strings.Repeat("0", 64)}
	return i.Validate()
}
func Hash(fields ...string) string {
	h := sha256.New()
	for _, s := range fields {
		fmt.Fprintf(h, "%d:", len(s))
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func PayloadDigest(p Payload) string            { return Hash(Protocol+"/payload", p.Action, p.TargetNode) }
func OutcomeDigest(outcome, code string) string { return Hash(Protocol+"/outcome", outcome, code) }
func (r Request) SemanticDigest() string {
	i := r.Identity
	return Hash(Protocol+"/operation", r.Operation, i.Protocol, i.OrgID, i.ControlPlaneID, i.TargetNode, i.APIKeyID, strconv.FormatInt(i.BindingVersion, 10), strconv.FormatInt(i.AuthorizationGeneration, 10), i.AttemptID, i.JobID, i.Queue, i.Group, strconv.FormatInt(i.ExpiresAt, 10), i.PayloadDigest, r.ProcessID, r.ClaimEpoch, r.OutcomeDigest)
}
func (r Request) TransportDigest() string {
	d := r.Delivery
	return Hash(Protocol+"/request", r.RequestID, r.OperationDigest, d.AssociationID, d.Queue, d.Group, d.MessageID)
}
func (r *Request) Seal() {
	r.OperationDigest = r.SemanticDigest()
	r.RequestDigest = r.TransportDigest()
}
func (r Request) Validate() error {
	if r.Identity.Validate() != nil || r.Delivery.Validate(r.Identity) != nil || !RandomUUID(r.RequestID) || !RandomUUID(r.ProcessID) {
		return ErrInvalid
	}
	switch r.Operation {
	case "claim_and_ack":
		if r.ClaimEpoch != "" || r.Outcome != "" || r.Code != "" || r.OutcomeDigest != "" {
			return ErrInvalid
		}
	case "complete":
		if !RandomUUID(r.ClaimEpoch) || !codeText.MatchString(r.Code) || (r.Outcome != "prepared" && r.Outcome != "refused_no_effect") || (r.Outcome == "prepared" && r.Code != "prepared") || r.OutcomeDigest != OutcomeDigest(r.Outcome, r.Code) {
			return ErrInvalid
		}
	case "read_status":
		if (r.ClaimEpoch != "" && !RandomUUID(r.ClaimEpoch)) || r.Outcome != "" || r.Code != "" || r.OutcomeDigest != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if r.OperationDigest != r.SemanticDigest() || r.RequestDigest != r.TransportDigest() {
		return ErrInvalid
	}
	return nil
}
func state(s string) bool {
	return s == "pending" || s == "claimed" || s == "refused_no_effect" || s == "commit_authorized" || s == "unconfirmed" || s == "resolved"
}

// ValidateResponse validates history and fresh observation separately. It never
// grants execution by itself; the live local reservation/deadline also must win.
func ValidateResponse(r Request, s Response) error {
	if r.Validate() != nil || s.RequestID != r.RequestID || s.Operation != r.Operation || s.Identity != r.Identity || s.Delivery != r.Delivery || s.ProcessID != r.ProcessID || s.OperationDigest != r.OperationDigest || s.RequestDigest != r.RequestDigest || !codeText.MatchString(s.Code) {
		return ErrInvalid
	}
	o := s.Observation
	if o != nil && (!integer(o.ServerTimeMS) || !integer(o.StateVersion) || !integer(o.BindingVersion) || !integer(o.AuthorizationGeneration) || !state(o.State)) {
		return ErrInvalid
	}
	if s.Decision == "denied" {
		if s.Receipt != nil || (o != nil && o.PermitUsable) {
			return ErrInvalid
		}
		return ErrDenied
	}
	if s.Decision == "unavailable" {
		if s.Receipt != nil || o != nil {
			return ErrInvalid
		}
		return ErrUnavailable
	}
	if s.Decision != "accepted" || s.Code != "ok" || o == nil || o.BindingVersion != r.Identity.BindingVersion || o.AuthorizationGeneration != r.Identity.AuthorizationGeneration {
		return ErrInvalid
	}
	p := s.Receipt
	if p == nil {
		if r.Operation != "read_status" || o.PermitUsable {
			return ErrInvalid
		}
		return nil
	}
	if !RandomUUID(p.ReceiptID) || !RandomUUID(p.ProcessID) || !RandomUUID(p.ClaimEpoch) || p.Delivery.Validate(r.Identity) != nil || !integer(p.StateVersion) || !integer(p.DecidedAtMS) || p.DecidedAtMS >= r.Identity.ExpiresAt || o.StateVersion < p.StateVersion || o.ServerTimeMS < p.DecidedAtMS {
		return ErrInvalid
	}
	historical := r
	historical.Operation = p.Operation
	historical.Delivery = p.Delivery
	historical.ProcessID = p.ProcessID
	historical.ClaimEpoch = p.ClaimEpoch
	historical.Outcome = p.Outcome
	historical.Code = p.Code
	historical.OutcomeDigest = p.OutcomeDigest
	if p.Operation == "claim_and_ack" {
		historical.ClaimEpoch = ""
		if p.Outcome != "" || p.Code != "" || p.OutcomeDigest != "" || p.State != "claimed" {
			return ErrInvalid
		}
	} else if p.Operation == "complete" {
		if !codeText.MatchString(p.Code) || p.OutcomeDigest != OutcomeDigest(p.Outcome, p.Code) || (p.Outcome != "prepared" && p.Outcome != "refused_no_effect") || (p.Outcome == "prepared" && (p.Code != "prepared" || p.State != "commit_authorized")) || (p.Outcome == "refused_no_effect" && p.State != "refused_no_effect") {
			return ErrInvalid
		}
	} else {
		return ErrInvalid
	}
	if p.OperationDigest != historical.SemanticDigest() {
		return ErrInvalid
	}
	if r.Operation == "read_status" {
		if o.PermitUsable {
			return ErrInvalid
		}
		return nil
	}
	if p.Operation != r.Operation || p.OperationDigest != r.OperationDigest || p.ProcessID != r.ProcessID || p.Delivery != r.Delivery || p.Outcome != r.Outcome || p.Code != r.Code || p.OutcomeDigest != r.OutcomeDigest {
		return ErrInvalid
	}
	if r.Operation == "claim_and_ack" {
		if o.PermitUsable || o.State != "claimed" {
			return ErrInvalid
		}
		return nil
	}
	if p.ClaimEpoch != r.ClaimEpoch {
		return ErrInvalid
	}
	if r.Outcome == "prepared" {
		if o.State != "commit_authorized" || !o.PermitUsable {
			return ErrInvalid
		}
	} else if o.State != "refused_no_effect" || o.PermitUsable {
		return ErrInvalid
	}
	return nil
}
