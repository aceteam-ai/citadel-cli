package serviceborrow

import (
	"errors"
	"fmt"
)

// Authorization refusal reasons. They distinguish the two refusal CLASSES the
// adversarial tests pin: "unverified_peer" (we could not establish a trusted
// mesh identity at all — resolver failure, nil identity, or #1068's empty
// stable/owner id) versus "not_authorized" (a fully verified identity that is
// simply not permitted — a shared-org or foreign peer). The classes matter
// because an operator debugging a denied borrow needs to know whether the mesh
// identity failed or the policy did.
const (
	// ReasonUnverified means the caller could not be verified as a trusted mesh
	// peer. The gate fails CLOSED here before the allowlist is even consulted:
	// an allowlisted login never bypasses mesh verification.
	ReasonUnverified = "unverified_peer"
	// ReasonNotAuthorized means the caller IS a verified mesh peer but is
	// neither same-owner nor on the platform infra allowlist (a shared-org or
	// foreign-owner node).
	ReasonNotAuthorized = "not_authorized"
)

// AuthzError is a typed authorization refusal from the serving gate. Reason is
// one of the ReasonXxx constants; Detail is a human-readable explanation for
// the audit log. It is never returned on success.
type AuthzError struct {
	Reason string
	Detail string
}

func (e *AuthzError) Error() string {
	if e.Detail == "" {
		return "serviceborrow: " + e.Reason
	}
	return "serviceborrow: " + e.Reason + ": " + e.Detail
}

// IsUnverified / IsNotAuthorized classify an AuthzError without string matching.
func (e *AuthzError) IsUnverified() bool    { return e.Reason == ReasonUnverified }
func (e *AuthzError) IsNotAuthorized() bool { return e.Reason == ReasonNotAuthorized }

// IdentityMismatchError is the typed refusal a requester returns when a peer's
// advertised service identity does not satisfy what the requester requires. It
// names the first differing field so the refusal is precise and debuggable.
type IdentityMismatchError struct {
	Field string
	Want  string
	Got   string
}

func (e *IdentityMismatchError) Error() string {
	return fmt.Sprintf("serviceborrow: service identity mismatch on %s: want %q, peer advertised %q", e.Field, e.Want, e.Got)
}

// ErrServiceUnavailable is the TYPED terminal error a requester returns when a
// service can be served neither by a verified matching peer nor locally (the
// local identity does not match what is wanted). Its message is a JSON object
// so a backend/orchestrator can branch on `reason`, mirroring
// jobs.ErrReceiptSigningUnavailable. It deliberately conveys "not available on
// THIS fabric path" and never degrades to a central/hosted fallback.
var ErrServiceUnavailable = errors.New(`{"reason":"service_unavailable","message":"This service is not available from a same-owner peer over the mesh and no matching local service is running. It is never served centrally or from a hosted endpoint."}`)
