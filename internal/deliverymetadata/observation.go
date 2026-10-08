// Package deliverymetadata retains passive observations, not execution authority.
package deliverymetadata

import (
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// State describes observed syntax only. No state validates a live delivery claim.
type State string

const (
	Unknown     State = "unknown_capture"
	Absent      State = "absent"
	Observed    State = "observed_v1"
	Incomplete  State = "incomplete_v1"
	Unsupported State = "unsupported_protocol"
	Ambiguous   State = "ambiguous_metadata"
)

// Provenance distinguishes original unique scopes from uncertain construction.
type Provenance string

const (
	Original  Provenance = "original_single_scope"
	Repeated  Provenance = "repeated_container"
	Uncertain Provenance = "scanner_unknown"
)

// Presence is a static classification; it never contains incoming data.
type Presence string

const (
	Missing      Presence = "missing"
	Captured     Presence = "captured_string"
	Null         Presence = "null_value"
	WrongType    Presence = "wrong_type"
	EncodedLimit Presence = "encoded_over_limit"
	Invalid      Presence = "decoded_invalid"
	Duplicate    Presence = "duplicate_member"
)

// Identity is a value snapshot, not a server proof. Unknown server identities
// (consumer, organization, device, socket and expiry) intentionally have no field.
type Identity struct {
	JobID, Type, MessageID, Queue, RequestedGroup, Transport string
}

// Observation is opaque and immutable. Its credential has no exported accessor.
type Observation struct {
	// fmt's bad-verb diagnostics can bypass Formatter on private pointer
	// fields. A function is opaque to reflection, including those diagnostics.
	snapshot func() ownedRecord
}

type ownedRecord struct {
	values          [5]string
	presence        [5]Presence
	provenance      Provenance
	identity        Identity
	identityInvalid bool
}

// freeze captures ONLY a final owned value, never the mutable scanner builder or
// original input. Clones may safely share this strictly immutable function.
func freeze(record ownedRecord) *Observation {
	return &Observation{snapshot: func() ownedRecord { return record }}
}

func (o *Observation) record() ownedRecord {
	if o == nil || o.snapshot == nil {
		return *empty("")
	}
	return o.snapshot()
}

const redacted = "<delivery-observation>"

// Format redacts every verb, including unsupported verbs and diagnostic formats.
func (Observation) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, redacted) }
func (Observation) String() string             { return redacted }
func (Observation) GoString() string           { return redacted }

// Clone returns an independent wrapper; strings are owned immutable captures.
func (o *Observation) Clone() *Observation {
	if o == nil {
		return nil
	}
	copy := *o
	return &copy
}

// Identity returns a value copy of the original captured identity and local context.
func (o *Observation) Identity() Identity {
	if o == nil {
		return Identity{}
	}
	return o.record().identity
}

func (o *Observation) Provenance() Provenance {
	if o == nil {
		return Uncertain
	}
	return o.record().provenance
}

// Presence reports only allowlisted names and static classifications.
func (o *Observation) Presence(name string) Presence {
	if o == nil {
		return Missing
	}
	for i, key := range names {
		if name == key {
			return o.record().presence[i]
		}
	}
	return Missing
}

// WithSource attaches local request context without rebinding any wire identity.
// HTTP has no wire queue; WS always preserves its original frame queue.
func (o *Observation) WithSource(queue, group, transport string) *Observation {
	if o == nil {
		return nil
	}
	copy := o.record()
	if transport == "redis-api" && copy.identity.Transport == "redis-api" {
		copy.identity.Queue, copy.identityInvalid = boundedIdentity(queue, 256, false, copy.identityInvalid)
	}
	copy.identity.RequestedGroup, copy.identityInvalid = boundedIdentity(group, 64, false, copy.identityInvalid)
	if transport != copy.identity.Transport {
		copy.identityInvalid = true
	}
	return freeze(copy)
}

func boundedIdentity(value string, cap int, ascii bool, invalid bool) (string, bool) {
	if value == "" || len(value) > cap || (ascii && !isASCII(value)) {
		return "", true
	}
	return strings.Clone(value), invalid
}

// State is passive lexical bookkeeping, never permission to execute or ACK.
func (observation *Observation) State() State {
	o := observation.record()
	if o.provenance == Uncertain {
		return Unknown
	}
	if o.provenance == Repeated {
		return Ambiguous
	}
	allMissing := true
	for _, p := range o.presence {
		if p == Duplicate {
			return Ambiguous
		}
		allMissing = allMissing && p == Missing
	}
	if allMissing {
		return Absent
	}
	if o.presence[0] != Captured || o.values[0] == "" {
		return Incomplete
	}
	if o.values[0] != "durable-results-v1" {
		return Unsupported
	}
	for _, p := range o.presence {
		if p != Captured {
			return Incomplete
		}
	}
	if o.identityInvalid || !canonicalDispatch(o.values[1]) || o.identity.JobID != o.values[1] || !lowerHex(o.values[2]) || !canonicalClaim(o.values[3]) || !canonicalEpoch(o.values[4]) {
		return Incomplete
	}
	return Observed
}

func canonicalDispatch(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}
func lowerHex(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func canonicalClaim(v string) bool {
	if len(v) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(v)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == v
}
func canonicalEpoch(v string) bool {
	if v == "" || v[0] < '1' || v[0] > '9' {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(v, 10, 64)
	return err == nil && n <= 9007199254740991
}
func isASCII(v string) bool {
	for i := range v {
		if v[i] > 127 {
			return false
		}
	}
	return true
}
