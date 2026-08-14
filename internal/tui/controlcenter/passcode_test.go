package controlcenter

import (
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// newTestControlCenterWithPermissions wires a ControlCenter to an in-memory
// permissions store, mirroring newTestSettings in settings_page_test.go so
// the passcode logic can be exercised without a running tview app.
func newTestControlCenterWithPermissions() (*ControlCenter, *config.Permissions) {
	store := config.DefaultPermissions()
	cfg := Config{
		Version: "1.0.0",
		Permissions: PermissionsCallbacks{
			Load: func() *config.Permissions {
				cp := *store
				return &cp
			},
			Save: func(p *config.Permissions) error {
				*store = *p
				return nil
			},
		},
	}
	cc := New(cfg)
	return cc, store
}

func TestSetNodePasscode_PersistsHashNotPlaintext(t *testing.T) {
	cc, store := newTestControlCenterWithPermissions()

	if store.HasPasscode() {
		t.Fatalf("expected no passcode set by default, got %+v", store)
	}

	perms, err := cc.setNodePasscode("123456")
	if err != nil {
		t.Fatalf("setNodePasscode returned error: %v", err)
	}
	if !perms.HasPasscode() {
		t.Error("returned permissions should report HasPasscode true after set")
	}
	if !store.HasPasscode() {
		t.Error("persisted store should have a passcode set")
	}
	if store.PasscodeHash == "123456" {
		t.Error("plaintext passcode must never be persisted verbatim")
	}
	if !store.VerifyPasscode("123456") {
		t.Error("stored hash should verify against the passcode that was set")
	}
	if store.VerifyPasscode("000000") {
		t.Error("stored hash should not verify against a wrong passcode")
	}
}

func TestSetNodePasscode_RotateReplacesPrevious(t *testing.T) {
	cc, store := newTestControlCenterWithPermissions()

	if _, err := cc.setNodePasscode("111111"); err != nil {
		t.Fatalf("initial set failed: %v", err)
	}
	if !store.VerifyPasscode("111111") {
		t.Fatal("expected first passcode to verify before rotation")
	}

	if _, err := cc.setNodePasscode("222222"); err != nil {
		t.Fatalf("rotate failed: %v", err)
	}
	if store.VerifyPasscode("111111") {
		t.Error("old passcode should no longer verify after rotation")
	}
	if !store.VerifyPasscode("222222") {
		t.Error("new passcode should verify after rotation")
	}
}

func TestClearNodePasscode_RemovesHashAndFailsClosed(t *testing.T) {
	cc, store := newTestControlCenterWithPermissions()

	if _, err := cc.setNodePasscode("654321"); err != nil {
		t.Fatalf("initial set failed: %v", err)
	}
	if !store.HasPasscode() {
		t.Fatal("expected passcode set before clearing")
	}

	perms, err := cc.clearNodePasscode()
	if err != nil {
		t.Fatalf("clearNodePasscode returned error: %v", err)
	}
	if perms.HasPasscode() {
		t.Error("returned permissions should report HasPasscode false after clear")
	}
	if store.HasPasscode() {
		t.Error("persisted store should have no passcode after clear")
	}
	if store.VerifyPasscode("654321") {
		t.Error("cleared passcode must fail closed: old PIN must not verify")
	}
	if store.VerifyPasscode("") {
		t.Error("cleared passcode must fail closed: empty PIN must not verify")
	}
}

func TestSetNodePasscode_NotConfigured(t *testing.T) {
	cc := New(Config{Version: "1.0.0"}) // Permissions callbacks left nil

	if _, err := cc.setNodePasscode("123456"); err == nil {
		t.Error("expected an error when permissions callbacks are not configured")
	}
	if _, err := cc.clearNodePasscode(); err == nil {
		t.Error("expected an error when permissions callbacks are not configured")
	}
}

func TestSetNodePasscode_SaveErrorPropagates(t *testing.T) {
	saveErr := errors.New("disk full")
	cfg := Config{
		Version: "1.0.0",
		Permissions: PermissionsCallbacks{
			Load: config.DefaultPermissions,
			Save: func(*config.Permissions) error { return saveErr },
		},
	}
	cc := New(cfg)

	if _, err := cc.setNodePasscode("123456"); err == nil {
		t.Error("expected setNodePasscode to propagate the save error")
	}
	if _, err := cc.clearNodePasscode(); err == nil {
		t.Error("expected clearNodePasscode to propagate the save error")
	}
}

func TestValidatePasscodeInput(t *testing.T) {
	cases := []struct {
		name    string
		pin     string
		confirm string
		wantErr bool
	}{
		{"matching", "123456", "123456", false},
		{"matching with surrounding whitespace", "  123456  ", "123456", false},
		{"empty pin", "", "123456", true},
		{"whitespace-only pin", "   ", "123456", true},
		{"mismatch", "123456", "654321", true},
		{"empty confirm", "123456", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePasscodeInput(tc.pin, tc.confirm)
			if tc.wantErr && err == nil {
				t.Errorf("validatePasscodeInput(%q, %q) = nil, want error", tc.pin, tc.confirm)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validatePasscodeInput(%q, %q) = %v, want nil", tc.pin, tc.confirm, err)
			}
		})
	}
}

func TestPasscodeClearNeedsWarning(t *testing.T) {
	cases := []struct {
		name  string
		perms *config.Permissions
		want  bool
	}{
		{"nothing sensitive enabled", &config.Permissions{}, false},
		{"console enabled", &config.Permissions{Console: true}, true},
		{"desktop enabled", &config.Permissions{Desktop: true}, true},
		{"files enabled", &config.Permissions{Files: true}, true},
		{"non-sensitive surfaces only", &config.Permissions{Services: true, SSH: true, Provision: true}, false},
		{"mixed sensitive and non-sensitive", &config.Permissions{Console: true, Services: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := passcodeClearNeedsWarning(tc.perms); got != tc.want {
				t.Errorf("passcodeClearNeedsWarning(%+v) = %v, want %v", tc.perms, got, tc.want)
			}
		})
	}
}
