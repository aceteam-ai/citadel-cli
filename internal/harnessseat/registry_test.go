package harnessseat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func registryFixture(t *testing.T) ([]byte, func(string) (string, error)) {
	t.Helper()
	home := t.TempDir()
	raw := fmt.Sprintf("profiles: {fixture-profile: {enabled: false, roots: [fixture-root]}}\nseats:\n  - id: fixture-seat\n    harness: codex\n    os_user: fixture-user\n    config_dir: %q\n    label: Personal\n    entitlement: consumer\n    roots: [%q]\n    data_ceiling: public\n    enabled: false\n", filepath.Join(home, "seat"), filepath.Join(home, "checkout"))
	return []byte(raw), func(owner string) (string, error) {
		if owner != "fixture-user" {
			return "", fmt.Errorf("fixture-owner-refusal")
		}
		return home, nil
	}
}

func TestRegistryRoundTripPreservesProfilesAndLocalDisable(t *testing.T) {
	raw, resolve := registryFixture(t)
	doc, err := DecodeRegistry(raw, resolve)
	if err != nil {
		t.Fatal(err)
	}
	seats := doc.Seats()
	if len(seats) != 1 || seats[0].Enabled || seats[0].Auth != AuthUnknown {
		t.Fatal("local state/defaults changed")
	}
	seats[0].Label = "Updated"
	encoded, err := doc.WithSeats(seats).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte("fixture-profile:")) || !bytes.Contains(encoded, []byte("fixture-root")) {
		t.Fatal("profile data dropped")
	}
	reloaded, err := DecodeRegistry(encoded, resolve)
	if err != nil || reloaded.Seats()[0].Label != "Updated" || reloaded.Seats()[0].Enabled {
		t.Fatal("seat round trip failed")
	}
	// Returned slices must not mutate a previously decoded declaration.
	seats[0].Roots[0] = "altered"
	if doc.Seats()[0].Roots[0] == "altered" {
		t.Fatal("mutable roots escaped snapshot")
	}
	data, err := json.Marshal(doc)
	if err != nil || string(data) != "{}" {
		t.Fatal("local document exposed through JSON")
	}
}

func TestRegistryRejectsAmbiguousOrUnsafeDeclarations(t *testing.T) {
	raw, resolve := registryFixture(t)
	home, _ := resolve("fixture-user")
	for name, source := range map[string]string{
		"duplicate-root-key":         string(raw) + "seats: []\n",
		"duplicate-seat-key":         strings.Replace(string(raw), "    enabled: false", "    enabled: false\n    enabled: true", 1),
		"alias":                      "profiles: &shared {}\nseats: *shared\n",
		"multiple-documents":         string(raw) + "---\nseats: []\n",
		"unknown-seat-field":         string(raw) + "    token: fixture-token\n",
		"unknown-owner":              strings.Replace(string(raw), "os_user: fixture-user", "os_user: missing-user", 1),
		"unknown-harness":            strings.Replace(string(raw), "harness: codex", "harness: unknown", 1),
		"coerced-enabled":            strings.Replace(string(raw), "    enabled: false", "    enabled: 'false'", 1),
		"missing-enabled":            strings.Replace(string(raw), "    enabled: false\n", "", 1),
		"unknown-entitlement":        strings.Replace(string(raw), "entitlement: consumer", "entitlement: unknown", 1),
		"signed-in-without-identity": string(raw) + "    auth: signed_in\n",
		"muse-private":               strings.Replace(strings.Replace(string(raw), "harness: codex", "harness: muse", 1), "data_ceiling: public", "data_ceiling: private", 1),
		"relative-root":              strings.Replace(string(raw), fmt.Sprintf("roots: [%q]", filepath.Join(home, "checkout")), "roots: [relative]", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRegistry([]byte(source), resolve); err == nil {
				t.Fatal("unsafe document accepted")
			}
		})
	}
	if _, err := DecodeRegistry(bytes.Repeat([]byte("x"), MaxRegistryBytes+1), resolve); err == nil {
		t.Fatal("unbounded document accepted")
	}
}

func TestRegistryRejectsDuplicateAccountsAndSharedConfigHomes(t *testing.T) {
	raw, resolve := registryFixture(t)
	doc, err := DecodeRegistry(raw, resolve)
	if err != nil {
		t.Fatal(err)
	}
	first := doc.Seats()[0]
	first.AccountDigest = strings.Repeat("a", 64)
	second := first
	second.ID, second.ConfigDir = "other-seat", filepath.Join(first.HomeDir, "other-seat")
	if _, err := doc.WithSeats([]Seat{first, second}).Encode(); err != ErrDuplicateAccount {
		t.Fatalf("duplicate account: %v", err)
	}
	second.AccountDigest, second.ConfigDir = "", first.ConfigDir
	if _, err := doc.WithSeats([]Seat{first, second}).Encode(); err == nil {
		t.Fatal("shared config home accepted")
	}
	second.ConfigDir = filepath.Join(first.ConfigDir, "nested")
	if _, err := doc.WithSeats([]Seat{first, second}).Encode(); err == nil {
		t.Fatal("nested config homes accepted")
	}
}

func TestRegistryProjectionOmitsLocalIdentityAndCopiesModels(t *testing.T) {
	raw, resolve := registryFixture(t)
	doc, err := DecodeRegistry(raw, resolve)
	if err != nil {
		t.Fatal(err)
	}
	seat := doc.Seats()[0]
	seat.Models, seat.PlanType = []string{"fixture-model"}, "fixture-plan"
	seat.AccountDigest = strings.Repeat("a", 64)
	p, err := seat.PublicProjection(bytes.Repeat([]byte{3}, 32), "fixture-node")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal([]any{seat, p})
	if err != nil {
		t.Fatal(err)
	}
	for _, local := range []string{seat.ID, seat.Owner, seat.ConfigDir, seat.HomeDir, seat.Roots[0], seat.AccountDigest, "fixture-node"} {
		if bytes.Contains(encoded, []byte(local)) {
			t.Fatal("local identity leaked")
		}
	}
	p.Models[0] = "changed"
	if seat.Models[0] == "changed" {
		t.Fatal("projection shared mutable model data")
	}
}
