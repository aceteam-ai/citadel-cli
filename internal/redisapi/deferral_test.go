package redisapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const deferralTestOperationID = "018f47a0-2d42-7c11-8d6b-2c5f93712f00"

type panicContext struct{}

func (panicContext) Deadline() (time.Time, bool) { panic("deferral source inspected context") }
func (panicContext) Done() <-chan struct{}       { panic("deferral source inspected context") }
func (panicContext) Err() error                  { panic("deferral source inspected context") }
func (panicContext) Value(any) any               { panic("deferral source inspected context") }

func TestDisabledDeferralSourceRefusesEveryVerbBeforeHandling(t *testing.T) {
	t.Setenv("CITADEL_R2_ENABLED", "true")
	t.Setenv("CITADEL_DEFERRAL_TOKEN", "credential-must-not-appear")

	source := NewDeferralSource()
	ctx := panicContext{}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "register", call: func() error { _, err := source.Register(ctx, DeferralRegisterRequest{}); return err }},
		{name: "consume", call: func() error { _, err := source.Consume(ctx, DeferralConsumeRequest{}); return err }},
		{name: "release", call: func() error { _, err := source.Release(ctx, DeferralReleaseRequest{}); return err }},
		{name: "start", call: func() error { _, err := source.Start(ctx, DeferralStartRequest{}); return err }},
		{name: "settle", call: func() error { _, err := source.Settle(ctx, DeferralSettleRequest{}); return err }},
		{name: "recover", call: func() error { _, err := source.Recover(ctx, DeferralRecoverRequest{}); return err }},
		{name: "status", call: func() error { _, err := source.Status(ctx, DeferralStatusRequest{}); return err }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if !errors.Is(err, ErrR2NotActivated) {
				t.Fatalf("error = %v, want ErrR2NotActivated", err)
			}
			var protocolError *DeferralError
			if !errors.As(err, &protocolError) {
				t.Fatalf("error type = %T, want *DeferralError", err)
			}
			if got := protocolError.Code(); got != DeferralErrorR2NotActivated {
				t.Fatalf("error code = %q, want %q", got, DeferralErrorR2NotActivated)
			}
			if got := err.Error(); got != "r2_not_activated" {
				t.Fatalf("error text = %q, want fixed refusal", got)
			}
			if strings.Contains(err.Error(), "credential-must-not-appear") {
				t.Fatal("refusal reflected credential material")
			}
		})
	}
}

func TestDeferralOperationIDIsCanonicalAndInternal(t *testing.T) {
	id, err := ParseDeferralOperationID(deferralTestOperationID)
	if err != nil {
		t.Fatalf("ParseDeferralOperationID() error = %v", err)
	}
	if got := id.String(); got != deferralTestOperationID {
		t.Fatalf("operation ID = %q, want %q", got, deferralTestOperationID)
	}

	requests := []interface{ OperationID() DeferralOperationID }{
		NewDeferralRegisterRequest(id),
		NewDeferralConsumeRequest(id),
		NewDeferralReleaseRequest(id),
		NewDeferralStartRequest(id),
		NewDeferralSettleRequest(id),
		NewDeferralRecoverRequest(id),
		NewDeferralStatusRequest(id),
	}
	for _, request := range requests {
		if got := request.OperationID(); got != id {
			t.Fatalf("%T operation ID = %q, want %q", request, got.String(), id.String())
		}
	}

	invalid := []string{
		"",
		"00000000-0000-0000-0000-000000000000",
		"018F47A0-2D42-7C11-8D6B-2C5F93712F00",
		"{018f47a0-2d42-7c11-8d6b-2c5f93712f00}",
		"018f47a0-2d42-7c11-0d6b-2c5f93712f00", // NCS variant.
		"018f47a0-2d42-7c11-cd6b-2c5f93712f00", // Microsoft variant.
		"018f47a0-2d42-7c11-ed6b-2c5f93712f00", // Reserved future variant.
		"018f47a0-2d42-0c11-8d6b-2c5f93712f00", // Version 0.
		"018f47a0-2d42-9c11-8d6b-2c5f93712f00", // Version 9.
		"not-an-operation-id",
	}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			_, err := ParseDeferralOperationID(value)
			if !errors.Is(err, ErrInvalidDeferralOperationID) {
				t.Fatalf("error = %v, want ErrInvalidDeferralOperationID", err)
			}
			if strings.Contains(err.Error(), value) && value != "" {
				t.Fatal("validation error reflected rejected input")
			}
		})
	}
}

func TestDeferralTypesRefuseWireSerialization(t *testing.T) {
	id, err := ParseDeferralOperationID(deferralTestOperationID)
	if err != nil {
		t.Fatalf("ParseDeferralOperationID() error = %v", err)
	}

	values := []any{
		id,
		NewDeferralRegisterRequest(id),
		NewDeferralConsumeRequest(id),
		NewDeferralReleaseRequest(id),
		NewDeferralStartRequest(id),
		NewDeferralSettleRequest(id),
		NewDeferralRecoverRequest(id),
		NewDeferralStatusRequest(id),
		DeferralRegisterResponse{},
		DeferralConsumeResponse{},
		DeferralReleaseResponse{},
		DeferralStartResponse{},
		DeferralSettleResponse{},
		DeferralRecoverResponse{},
		DeferralStatusResponse{},
	}
	for _, value := range values {
		_, err := json.Marshal(value)
		if !errors.Is(err, ErrDeferralWireSchemaUnavailable) {
			t.Fatalf("json.Marshal(%T) error = %v, want ErrDeferralWireSchemaUnavailable", value, err)
		}
	}

	var request DeferralRegisterRequest
	if err := json.Unmarshal([]byte(`{"operationId":"not-a-contract"}`), &request); !errors.Is(err, ErrDeferralWireSchemaUnavailable) {
		t.Fatalf("json.Unmarshal() error = %v, want ErrDeferralWireSchemaUnavailable", err)
	}
}

func TestDurableDeferralSourceOnlyFixture(t *testing.T) {
	type fixture struct {
		Scope                 string   `json:"scope"`
		WireSchema            string   `json:"wireSchema"`
		ResponseAuthority     string   `json:"responseAuthority"`
		Activation            string   `json:"activation"`
		ErrorCode             string   `json:"errorCode"`
		Verbs                 []string `json:"verbs"`
		ForbiddenActivationBy []string `json:"forbiddenActivationBy"`
		IOBeforeRefusal       []string `json:"ioBeforeRefusal"`
	}

	data, err := os.ReadFile("../worker/testdata/durable_deferral_v1.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var got fixture
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	want := fixture{
		Scope:                 "internal_source_only",
		WireSchema:            "unavailable",
		ResponseAuthority:     "unavailable",
		Activation:            "disabled",
		ErrorCode:             "r2_not_activated",
		Verbs:                 []string{"register", "consume", "release", "start", "settle", "recover", "status"},
		ForbiddenActivationBy: []string{"environment", "header", "config"},
		IOBeforeRefusal:       []string{"http", "websocket", "auth", "redis", "queue", "coordinator"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture = %#v, want %#v", got, want)
	}
}

var _ context.Context = panicContext{}
