package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestDeviceEnrollmentLinkUsesRegisteredIdentityAndCurrentGrant(t *testing.T) {
	result := &DeviceAuthResult{DeviceCode: "current-grant", Token: &nexus.TokenResponse{Authkey: "current-registration-key"}}
	nodeID := func(context.Context) string { return "42" }
	called := false
	refused := errors.New("reservation no longer owns this node")
	err := linkDeviceEnrollmentWith(context.Background(), result, nodeID, func(_ context.Context, code, id string) error {
		called = true
		if code != result.DeviceCode || id != "42" {
			t.Fatal("link substituted another enrollment or node identity")
		}
		return refused
	})
	if !called || !errors.Is(err, refused) {
		t.Fatal("server link refusal must propagate to the enrollment caller")
	}
}

func TestDeviceEnrollmentLinkRefusesMissingCurrentAuthorization(t *testing.T) {
	for _, result := range []*DeviceAuthResult{nil, {}, {Token: &nexus.TokenResponse{}}, {DeviceCode: "grant", Token: &nexus.TokenResponse{DeviceAPIToken: "saved-bearer-alone"}}} {
		err := linkDeviceEnrollmentWith(context.Background(), result, func(context.Context) string { t.Fatal("must reject before network identity lookup"); return "42" }, func(context.Context, string, string) error { t.Fatal("must not link"); return nil })
		if err == nil {
			t.Fatal("missing current authorization must fail")
		}
	}
	result := &DeviceAuthResult{DeviceCode: "grant", Token: &nexus.TokenResponse{Authkey: "registration-key"}}
	err := linkDeviceEnrollmentWith(context.Background(), result, func(context.Context) string { return "" }, func(context.Context, string, string) error {
		t.Fatal("must not substitute hostname or IP")
		return nil
	})
	if err == nil {
		t.Fatal("missing registered identity must fail")
	}
}

func TestDeviceEnrollmentRefusalExplainsFreshRegistrationRecovery(t *testing.T) {
	refused := errors.New("device reservation does not match the connected node")
	err := deviceEnrollmentRecoveryError(refused)
	if !errors.Is(err, refused) || !strings.Contains(err.Error(), "saved credentials do not complete device binding") || !strings.Contains(err.Error(), "citadel logout && citadel init") {
		t.Fatal("link refusal must preserve its cause and explain explicit fresh-registration recovery")
	}
}
