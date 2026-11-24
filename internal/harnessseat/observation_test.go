//go:build linux || darwin

package harnessseat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func observationFixture(t *testing.T) (*SaltStore, RegistrySnapshot, string) {
	t.Helper()
	s, dir := saltFixture(t)
	_, resolve := registryFixture(t)
	before, err := s.InitializeRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := resolve("fixture-user")
	seats := []Seat{}
	for _, id := range []string{"first-seat", "second-seat"} {
		seats = append(seats, Seat{Identity: Identity{ID: id, Harness: Codex, Owner: "fixture-user", HomeDir: home, ConfigDir: filepath.Join(home, id)}, Label: id, Entitlement: "consumer", Roots: []string{filepath.Join(home, "checkout")}, DataCeiling: "public", Auth: AuthUnknown})
	}
	before, err = s.UpdateRegistry(context.Background(), before, seats)
	if err != nil {
		t.Fatal(err)
	}
	return s, before, dir
}

func fixtureObservation(seat Seat) probeObservation {
	return probeObservation{identity: seat.Identity, auth: AuthSignedIn, entitlement: "workspace", planType: "fixture-team", models: []string{"fixture-model"}, accountID: "fixture-account-identity"}
}

func TestProbeObservationPersistsOnlyDigestAndObservedMetadata(t *testing.T) {
	s, before, dir := observationFixture(t)
	observation := fixtureObservation(before.Seats()[0])
	after, mismatch, err := s.reconcileProbeObservation(context.Background(), before, observation)
	if err != nil || !mismatch {
		t.Fatal("observed entitlement did not override declaration")
	}
	seat := after.Seats()[0]
	want, err := AccountDigest(before.salt[:], seat.Harness, observation.accountID)
	if err != nil || seat.AccountDigest != want || seat.Auth != AuthSignedIn || seat.Entitlement != "workspace" || seat.PlanType != "fixture-team" || len(seat.Models) != 1 || seat.Enabled {
		t.Fatal("observation did not preserve local disable or discovered metadata")
	}
	if seat.ConfigDir != before.Seats()[0].ConfigDir || seat.Roots[0] != before.Seats()[0].Roots[0] || seat.DataCeiling != "public" {
		t.Fatal("probe changed local policy or binding")
	}
	observation.models[0] = "mutated-model"
	if after.Seats()[0].Models[0] != "fixture-model" {
		t.Fatal("mutable probe result retained")
	}
	raw, err := os.ReadFile(filepath.Join(dir, registryFileName))
	if err != nil || bytes.Contains(raw, []byte(observation.accountID)) || !bytes.Contains(raw, []byte(want)) {
		t.Fatal("raw account identity persisted")
	}
	projection, err := seat.PublicProjection(before.salt[:], "fixture-node")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(projection)
	if bytes.Contains(encoded, []byte(want)) || bytes.Contains(encoded, []byte(observation.accountID)) {
		t.Fatal("report projection leaked identity")
	}
	if _, err := os.Stat(seat.ConfigDir); !os.IsNotExist(err) {
		t.Fatal("reconciliation touched vendor directory")
	}
}

func TestProbeObservationRejectsDuplicateAccountWithoutPartialWrite(t *testing.T) {
	s, before, _ := observationFixture(t)
	after, _, err := s.reconcileProbeObservation(context.Background(), before, fixtureObservation(before.Seats()[0]))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.reconcileProbeObservation(context.Background(), after, fixtureObservation(after.Seats()[1])); !errors.Is(err, ErrDuplicateAccount) {
		t.Fatal("duplicate account admitted")
	}
	current, err := s.LoadRegistry(context.Background(), after.document.resolve)
	if err != nil || current.Revision() != after.Revision() || current.Seats()[1].Auth != AuthUnknown || current.Seats()[1].AccountDigest != "" {
		t.Fatal("failed duplicate probe partially published")
	}
}

func TestProbeObservationAccountIdentityIsHarnessScoped(t *testing.T) {
	s, before, _ := observationFixture(t)
	seats := before.Seats()
	seats[1].Harness = Claude
	seats[0].Enabled = true
	before, err := s.UpdateRegistry(context.Background(), before, seats)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := s.reconcileProbeObservation(context.Background(), before, fixtureObservation(before.Seats()[0]))
	if err != nil || !after.Seats()[0].Enabled {
		t.Fatal("probe changed local enabled flag")
	}
	after, _, err = s.reconcileProbeObservation(context.Background(), after, fixtureObservation(after.Seats()[1]))
	if err != nil {
		t.Fatal("distinct harness namespace collided")
	}
	if after.Seats()[0].AccountDigest == after.Seats()[1].AccountDigest {
		t.Fatal("harness namespace missing from digest")
	}
}

func TestProbeObservationCannotUndoConcurrentLocalDisable(t *testing.T) {
	s, before, _ := observationFixture(t)
	seats := before.Seats()
	seats[0].Enabled = true
	before, err := s.UpdateRegistry(context.Background(), before, seats)
	if err != nil {
		t.Fatal(err)
	}
	observation := fixtureObservation(before.Seats()[0])
	disabled, err := s.SetLocalSeatEnabled(context.Background(), before, observation.identity.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.reconcileProbeObservation(context.Background(), before, observation); !errors.Is(err, ErrRegistryConflict) {
		t.Fatal("stale probe overwrote local disable")
	}
	after, _, err := s.reconcileProbeObservation(context.Background(), disabled, observation)
	if err != nil || after.Seats()[0].Enabled || after.Seats()[0].Auth != AuthSignedIn {
		t.Fatal("fresh probe re-enabled disabled seat")
	}
}

func TestProbeObservationUnknownAndSignedOutClearDerivedEvidence(t *testing.T) {
	for _, auth := range []AuthState{AuthUnknown, AuthSignedOut} {
		t.Run(string(auth), func(t *testing.T) {
			s, before, _ := observationFixture(t)
			signedIn, _, err := s.reconcileProbeObservation(context.Background(), before, fixtureObservation(before.Seats()[0]))
			if err != nil {
				t.Fatal(err)
			}
			after, mismatch, err := s.reconcileProbeObservation(context.Background(), signedIn, probeObservation{identity: signedIn.Seats()[0].Identity, auth: auth})
			if err != nil || mismatch {
				t.Fatal("could not record explicit non-signed-in state")
			}
			seat := after.Seats()[0]
			if seat.Auth != auth || seat.AccountDigest != "" || seat.PlanType != "" || len(seat.Models) != 0 || seat.Entitlement != "workspace" || seat.Enabled {
				t.Fatal("stale derived evidence or local flag changed")
			}
		})
	}
}

func TestProbeObservationRefusesInvalidOrWrongBinding(t *testing.T) {
	for _, kind := range []string{"harness", "owner", "home", "config", "missing", "account", "blank-account", "padded-account", "long-account", "control", "entitlement", "plan", "models", "many-models", "unknown-with-identity", "bad-auth", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			s, before, _ := observationFixture(t)
			observation := fixtureObservation(before.Seats()[0])
			ctx := context.Background()
			switch kind {
			case "harness":
				observation.identity.Harness = Claude
			case "owner":
				observation.identity.Owner = "other-user"
			case "home":
				observation.identity.HomeDir = filepath.Join(observation.identity.HomeDir, "other")
			case "config":
				observation.identity.ConfigDir += "-changed"
			case "missing":
				observation.identity.ID = "missing-seat"
			case "account":
				observation.accountID = ""
			case "blank-account":
				observation.accountID = "   "
			case "padded-account":
				observation.accountID += " "
			case "long-account":
				observation.accountID = strings.Repeat("a", 4097)
			case "control":
				observation.accountID = "fixture\nidentity"
			case "entitlement":
				observation.entitlement = "invalid"
			case "plan":
				observation.planType = "invalid plan"
			case "models":
				observation.models = []string{"duplicate", "duplicate"}
			case "many-models":
				observation.models = make([]string, 65)
			case "unknown-with-identity":
				observation.auth = AuthUnknown
			case "bad-auth":
				observation.auth = "trusted"
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, _, err := s.reconcileProbeObservation(ctx, before, observation); err == nil {
				t.Fatal("invalid observation accepted")
			}
			current, err := s.LoadRegistry(context.Background(), before.document.resolve)
			if err != nil || current.Revision() != before.Revision() {
				t.Fatal("refused observation changed registry")
			}
		})
	}
}

func TestProbeObservationConcurrentResultsCannotMultiplyAccount(t *testing.T) {
	s, before, dir := observationFixture(t)
	other, err := OpenSaltStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherSnapshot, err := other.LoadRegistry(context.Background(), before.document.resolve)
	if err != nil {
		t.Fatal(err)
	}
	stores := []*SaltStore{s, other}
	snapshots := []RegistrySnapshot{before, otherSnapshot}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, seat := range before.Seats() {
		wg.Add(1)
		go func(i int, seat Seat) {
			defer wg.Done()
			_, _, errs[i] = stores[i].reconcileProbeObservation(context.Background(), snapshots[i], fixtureObservation(seat))
		}(i, seat)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrRegistryConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("concurrent observations were not fenced")
	}
	current, err := s.LoadRegistry(context.Background(), before.document.resolve)
	if err != nil {
		t.Fatal(err)
	}
	for _, seat := range current.Seats() {
		if seat.Auth == AuthUnknown {
			if _, _, err := s.reconcileProbeObservation(context.Background(), current, fixtureObservation(seat)); !errors.Is(err, ErrDuplicateAccount) {
				t.Fatal("retry multiplied observed account")
			}
		}
	}
}

func TestProbeObservationRejectsForeignSnapshotAndChangedSalt(t *testing.T) {
	s, before, dir := observationFixture(t)
	observation := fixtureObservation(before.Seats()[0])
	other, err := OpenSaltStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, _, err := other.reconcileProbeObservation(context.Background(), before, observation); !errors.Is(err, ErrRegistryConflict) {
		t.Fatal("foreign store snapshot accepted")
	}
	raw, err := os.ReadFile(filepath.Join(dir, registryFileName))
	if err != nil {
		t.Fatal(err)
	}
	changed := before.salt
	changed[0] ^= 0xff
	if err := os.WriteFile(filepath.Join(dir, saltFileName), changed[:], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.reconcileProbeObservation(context.Background(), before, observation); !errors.Is(err, ErrRegistryConflict) {
		t.Fatal("probe rebound to replaced identity salt")
	}
	if after, err := os.ReadFile(filepath.Join(dir, registryFileName)); err != nil || !bytes.Equal(after, raw) {
		t.Fatal("salt refusal changed registry")
	}
}

func TestProbeObservationDebugAndJSONAreRedacted(t *testing.T) {
	_, before, _ := observationFixture(t)
	observation := fixtureObservation(before.Seats()[0])
	encoded, err := json.Marshal(observation)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("observation unexpectedly serializes")
	}
	debug := fmt.Sprintf("%v %+v %#v", observation, &observation, observation)
	for _, private := range []string{observation.accountID, observation.identity.ConfigDir, observation.identity.Owner} {
		if strings.Contains(debug, private) {
			t.Fatal("observation debug exposed private identity")
		}
	}
}
