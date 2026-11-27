//go:build linux || darwin

package harnessseat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func localAddFixture(t *testing.T) (*SaltStore, RegistrySnapshot, LocalSeatDeclaration) {
	t.Helper()
	s, _ := saltFixture(t)
	_, resolve := registryFixture(t)
	before, err := s.InitializeRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := resolve("fixture-user")
	return s, before, LocalSeatDeclaration{ID: "first-seat", Harness: Codex, Owner: "fixture-user", ConfigDir: filepath.Join(home, "seats", "first"), Label: "First", Entitlement: "consumer", Roots: []string{filepath.Join(home, "checkout")}, DataCeiling: "public"}
}

func TestLocalSeatEditsCreateDisabledUnknownDeclarationAndNeverDeleteVendorState(t *testing.T) {
	s, before, request := localAddFixture(t)
	added, err := s.AddLocalSeat(context.Background(), before, request)
	if err != nil {
		t.Fatal(err)
	}
	seat := added.Seats()[0]
	if seat.Enabled || seat.Auth != AuthUnknown || seat.AccountDigest != "" || seat.PlanType != "" || len(seat.Models) != 0 {
		t.Fatal("add invented observed login or enabled authority")
	}
	info, err := os.Stat(request.ConfigDir)
	if err != nil || info.Mode() != os.ModeDir|0o700 {
		t.Fatal("add did not make private directory")
	}
	marker := filepath.Join(request.ConfigDir, "untouched.fixture")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled, err := s.SetLocalSeatEnabled(context.Background(), added, request.ID, true)
	if err != nil || !enabled.Seats()[0].Enabled || enabled.Seats()[0].Auth != AuthUnknown {
		t.Fatal("local flag changed login evidence")
	}
	disabled, err := s.SetLocalSeatEnabled(context.Background(), enabled, request.ID, false)
	if err != nil || disabled.Seats()[0].Enabled {
		t.Fatal("disable failed")
	}
	removed, err := s.RemoveLocalSeat(context.Background(), disabled, request.ID)
	if err != nil || len(removed.Seats()) != 0 {
		t.Fatal("remove failed")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged" {
		t.Fatal("metadata edit touched vendor file")
	}
}

func TestLocalSeatEditsRefuseStaleOrInvalidAddBeforeCreatingDirectories(t *testing.T) {
	for _, kind := range []string{"stale", "invalid-owner", "invalid-root", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			s, before, request := localAddFixture(t)
			ctx := context.Background()
			switch kind {
			case "stale":
				// A real intervening metadata write, not a fabricated revision.
				home, _ := before.document.resolve(request.Owner)
				other := Seat{Identity: Identity{ID: "other-seat", Harness: request.Harness, Owner: request.Owner, ConfigDir: filepath.Join(home, "other-seat")}, Label: "Other", Entitlement: request.Entitlement, Roots: request.Roots, DataCeiling: request.DataCeiling, Auth: AuthUnknown}
				if _, err := s.UpdateRegistry(ctx, before, []Seat{other}); err != nil {
					t.Fatal(err)
				}
			case "invalid-owner":
				request.Owner = "missing-user"
			case "invalid-root":
				request.Roots = []string{"relative"}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := s.AddLocalSeat(ctx, before, request); err == nil {
				t.Fatal("invalid add accepted")
			}
			if _, err := os.Stat(filepath.Dir(request.ConfigDir)); !os.IsNotExist(err) {
				t.Fatal("refused add made directories")
			}
		})
	}
}

func TestLocalSeatEditsRefuseExistingLoginAndPermitDisableWithBrokenDirectory(t *testing.T) {
	s, before, request := localAddFixture(t)
	if err := os.MkdirAll(request.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLocalSeat(context.Background(), before, request); !errors.Is(err, ErrSeatDirectoryExists) {
		t.Fatal("existing login silently adopted")
	}
	// A pre-existing declaration may be disabled or removed even if its local
	// directory becomes unsafe. Enabling it must refuse without mode repair.
	seat := Seat{Identity: Identity{ID: request.ID, Harness: request.Harness, Owner: request.Owner, ConfigDir: request.ConfigDir}, Label: request.Label, Entitlement: request.Entitlement, Roots: request.Roots, DataCeiling: request.DataCeiling, Enabled: true, Auth: AuthUnknown}
	added, err := s.UpdateRegistry(context.Background(), before, []Seat{seat})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(request.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	disabled, err := s.SetLocalSeatEnabled(context.Background(), added, request.ID, false)
	if err != nil || disabled.Seats()[0].Enabled {
		t.Fatal("unsafe directory prevented disable")
	}
	if _, err := s.SetLocalSeatEnabled(context.Background(), disabled, request.ID, true); !errors.Is(err, ErrSeatDirectory) {
		t.Fatal("unsafe directory enabled")
	}
	if _, err := s.RemoveLocalSeat(context.Background(), disabled, request.ID); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(request.ConfigDir)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatal("edit repaired or deleted vendor directory")
	}
}

func TestLocalSeatEditsUnknownIDsAndStaleFlagsAreRefused(t *testing.T) {
	s, before, request := localAddFixture(t)
	if _, err := s.RemoveLocalSeat(context.Background(), before, "missing"); !errors.Is(err, ErrSeatNotFound) {
		t.Fatal("unknown removal accepted")
	}
	if _, err := s.SetLocalSeatEnabled(context.Background(), before, "missing", false); !errors.Is(err, ErrSeatNotFound) {
		t.Fatal("unknown disable accepted")
	}
	added, err := s.AddLocalSeat(context.Background(), before, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveLocalSeat(context.Background(), before, request.ID); !errors.Is(err, ErrRegistryConflict) {
		t.Fatal("stale removal accepted")
	}
	if _, err := s.SetLocalSeatEnabled(context.Background(), before, request.ID, true); !errors.Is(err, ErrRegistryConflict) {
		t.Fatal("stale flag accepted")
	}
	if _, err := s.AddLocalSeat(context.Background(), added, request); err == nil {
		t.Fatal("duplicate declaration accepted")
	}
}
