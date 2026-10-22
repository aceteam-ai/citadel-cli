package cmd

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

func capturePasscodeWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestPasscodeUnavailableWarningNeverFormatsWrappedError(t *testing.T) {
	output := capturePasscodeWarnings(t)
	warnPasscodeLockoutUnavailable(nodevault.ErrWrongPIN)
	if output.Len() != 0 {
		t.Fatal("ordinary mismatch emitted an operational warning")
	}
	warnPasscodeLockoutUnavailable(fmt.Errorf("%w: secret-pin-and-private-path", nodevault.ErrLockoutUnavailable))
	if strings.Count(output.String(), "level=WARN") != 1 || !strings.Contains(output.String(), "lockout state unavailable") || strings.Contains(output.String(), "secret-pin-and-private-path") {
		t.Fatalf("warning was missing, duplicated or disclosed wrapped details: %q", output.String())
	}
}

func TestProductionBooleanPasscodeGatesWarnOnUnavailableAccounting(t *testing.T) {
	for _, master := range []bool{false, true} {
		t.Run(fmt.Sprintf("master=%v", master), func(t *testing.T) {
			dir := useWiredLegacyLockout(t)
			output := capturePasscodeWarnings(t)
			permissions := config.DefaultPermissions()
			var statePath string
			if master {
				if err := nodevault.Open(dir).SetPIN("654321", nodevault.DefaultPolicy(), true); err != nil {
					t.Fatal(err)
				}
				statePath = filepath.Join(dir, "vault", "state.yaml")
			} else {
				if err := permissions.SetPasscode("654321"); err != nil {
					t.Fatal(err)
				}
				statePath = filepath.Join(dir, "passcode-lockout.yaml")
			}
			if err := os.Mkdir(statePath+".lock", 0o700); err != nil {
				t.Fatal(err)
			}
			for _, pin := range []string{"654321", "000000"} {
				if permissions.VerifyPasscode(pin) {
					t.Fatal("unavailable accounting granted boolean access")
				}
			}
			if strings.Count(output.String(), "level=WARN") != 2 || strings.Contains(output.String(), "654321") || strings.Contains(output.String(), "000000") || strings.Contains(output.String(), dir) {
				t.Fatalf("boolean gate warning count/privacy differs: %q", output.String())
			}
		})
	}
}

func TestPrintLockoutStatusDoesNotPresentUnavailableStateAsClean(t *testing.T) {
	output := captureStdoutForTest(t, func() {
		printLockoutStatus(nodevault.LockoutStatus{Unavailable: true, LockedOut: true, FailedAttempts: 99})
	})
	if !strings.Contains(output, "UNAVAILABLE") || !strings.Contains(output, "reset-lockout") || strings.Contains(output, "ACTIVE") || strings.Contains(output, "99") {
		t.Fatalf("unavailable status was misleading: %q", output)
	}
}
