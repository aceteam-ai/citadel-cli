package redisapi

import (
	"errors"

	"github.com/google/uuid"
)

// DeferralErrorCode identifies a durable-deferral protocol refusal without
// exposing request data or credentials in the error text.
type DeferralErrorCode string

const (
	// DeferralErrorR2NotActivated is the only production result in this source
	// slice. It is returned before any request handling or external I/O.
	DeferralErrorR2NotActivated DeferralErrorCode = "r2_not_activated"
)

// DeferralError is a typed durable-deferral protocol error.
type DeferralError struct {
	code DeferralErrorCode
}

// Error returns only the fixed protocol code. It never includes request or
// credential material.
func (e *DeferralError) Error() string {
	return string(e.code)
}

// Code returns the fixed protocol error code.
func (e *DeferralError) Code() DeferralErrorCode {
	return e.code
}

// ErrR2NotActivated is returned synchronously by every production deferral
// operation until the separately reviewed activation boundary exists.
var ErrR2NotActivated = &DeferralError{code: DeferralErrorR2NotActivated}

// ErrInvalidDeferralOperationID is returned for a non-canonical operation ID.
// The error deliberately does not echo the rejected value.
var ErrInvalidDeferralOperationID = errors.New("invalid durable deferral operation ID")

// ErrDeferralWireSchemaUnavailable prevents these internal protocol types from
// being mistaken for a frozen JSON or text transport contract.
var ErrDeferralWireSchemaUnavailable = errors.New("durable deferral wire schema unavailable")

type deferralNoWire struct{}

func (deferralNoWire) MarshalJSON() ([]byte, error) {
	return nil, ErrDeferralWireSchemaUnavailable
}

func (*deferralNoWire) UnmarshalJSON([]byte) error {
	return ErrDeferralWireSchemaUnavailable
}

// DeferralOperationID is a validated internal operation identity. It has no
// JSON or text marshaling contract: the transport wire schema is intentionally
// unavailable in this source slice.
type DeferralOperationID struct {
	deferralNoWire
	canonical string
}

// ParseDeferralOperationID accepts only a non-zero, lowercase canonical RFC
// 4122-variant UUID with version 1 through 8. It does not establish a public
// wire representation.
func ParseDeferralOperationID(value string) (DeferralOperationID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value ||
		parsed.Variant() != uuid.RFC4122 || parsed.Version() < 1 || parsed.Version() > 8 {
		return DeferralOperationID{}, ErrInvalidDeferralOperationID
	}
	return DeferralOperationID{canonical: value}, nil
}

// String returns the validated internal identity.
func (id DeferralOperationID) String() string {
	return id.canonical
}

// The request types below are deliberately distinct and non-serializable.
// Their only frozen field is the canonical internal operation identity. Future
// HTTP or WebSocket adapters must not infer a wire shape from these types.

type DeferralRegisterRequest struct {
	deferralNoWire
	operationID DeferralOperationID
}

type DeferralConsumeRequest struct {
	deferralNoWire
	operationID DeferralOperationID
}

type DeferralReleaseRequest struct {
	deferralNoWire
	operationID DeferralOperationID
}

type DeferralStartRequest struct {
	deferralNoWire
	operationID DeferralOperationID
}

type DeferralSettleRequest struct {
	deferralNoWire
	operationID DeferralOperationID
}

type DeferralRecoverRequest struct {
	deferralNoWire
	operationID DeferralOperationID
}

type DeferralStatusRequest struct {
	deferralNoWire
	operationID DeferralOperationID
}

func NewDeferralRegisterRequest(id DeferralOperationID) DeferralRegisterRequest {
	return DeferralRegisterRequest{operationID: id}
}

func NewDeferralConsumeRequest(id DeferralOperationID) DeferralConsumeRequest {
	return DeferralConsumeRequest{operationID: id}
}

func NewDeferralReleaseRequest(id DeferralOperationID) DeferralReleaseRequest {
	return DeferralReleaseRequest{operationID: id}
}

func NewDeferralStartRequest(id DeferralOperationID) DeferralStartRequest {
	return DeferralStartRequest{operationID: id}
}

func NewDeferralSettleRequest(id DeferralOperationID) DeferralSettleRequest {
	return DeferralSettleRequest{operationID: id}
}

func NewDeferralRecoverRequest(id DeferralOperationID) DeferralRecoverRequest {
	return DeferralRecoverRequest{operationID: id}
}

func NewDeferralStatusRequest(id DeferralOperationID) DeferralStatusRequest {
	return DeferralStatusRequest{operationID: id}
}

func (r DeferralRegisterRequest) OperationID() DeferralOperationID { return r.operationID }
func (r DeferralConsumeRequest) OperationID() DeferralOperationID  { return r.operationID }
func (r DeferralReleaseRequest) OperationID() DeferralOperationID  { return r.operationID }
func (r DeferralStartRequest) OperationID() DeferralOperationID    { return r.operationID }
func (r DeferralSettleRequest) OperationID() DeferralOperationID   { return r.operationID }
func (r DeferralRecoverRequest) OperationID() DeferralOperationID  { return r.operationID }
func (r DeferralStatusRequest) OperationID() DeferralOperationID   { return r.operationID }

// The response types are intentionally opaque. No response authority or wire
// representation exists before the later protocol and activation children.
type deferralResponseAuthorityUnavailable struct{}

type DeferralRegisterResponse struct {
	deferralNoWire
	unavailable deferralResponseAuthorityUnavailable
}

type DeferralConsumeResponse struct {
	deferralNoWire
	unavailable deferralResponseAuthorityUnavailable
}

type DeferralReleaseResponse struct {
	deferralNoWire
	unavailable deferralResponseAuthorityUnavailable
}

type DeferralStartResponse struct {
	deferralNoWire
	unavailable deferralResponseAuthorityUnavailable
}

type DeferralSettleResponse struct {
	deferralNoWire
	unavailable deferralResponseAuthorityUnavailable
}

type DeferralRecoverResponse struct {
	deferralNoWire
	unavailable deferralResponseAuthorityUnavailable
}

type DeferralStatusResponse struct {
	deferralNoWire
	unavailable deferralResponseAuthorityUnavailable
}
