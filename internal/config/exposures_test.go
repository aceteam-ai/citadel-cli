package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The bug in #647: an exposure lived only in the running worker, so a restart
// dropped it. Save -> Load must reproduce every field the gateway needs to
// rebuild both the route and its access policy.
func TestSaveLoadExposureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := ExposureRecord{
		Name:       "frigate",
		Port:       8212,
		Visibility: "link",
		Creator:    "org_abc123",
		TokenEpoch: 3,
	}
	if err := SaveExposure(dir, want); err != nil {
		t.Fatalf("SaveExposure: %v", err)
	}

	got, err := LoadExposures(dir)
	if err != nil {
		t.Fatalf("LoadExposures: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d records, want 1", len(got))
	}
	if got[0] != want {
		t.Errorf("round trip = %+v, want %+v", got[0], want)
	}
}

// TokenEpoch specifically must survive. If it reset to 0 on restart, every link
// token minted under the old epoch would stop verifying — live shared links
// would break — and a later epoch bump could no longer revoke them.
func TestTokenEpochSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	if err := SaveExposure(dir, ExposureRecord{Name: "x", Port: 1, Visibility: "link", TokenEpoch: 7}); err != nil {
		t.Fatal(err)
	}

	got, _ := LoadExposures(dir)
	if len(got) != 1 || got[0].TokenEpoch != 7 {
		t.Errorf("TokenEpoch = %v, want 7 preserved across the store", got)
	}
}

// Re-exposing the same name must REPLACE, not accumulate: the gateway keeps one
// policy per name, so a duplicate record would make the restore order decide
// which policy wins — and could silently reinstate an older, looser visibility.
func TestSaveExposureReplacesByName(t *testing.T) {
	dir := t.TempDir()
	if err := SaveExposure(dir, ExposureRecord{Name: "frigate", Port: 8212, Visibility: "link"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveExposure(dir, ExposureRecord{Name: "frigate", Port: 9000, Visibility: "org"}); err != nil {
		t.Fatal(err)
	}

	got, _ := LoadExposures(dir)
	if len(got) != 1 {
		t.Fatalf("loaded %d records, want 1 (re-expose must replace)", len(got))
	}
	if got[0].Visibility != "org" || got[0].Port != 9000 {
		t.Errorf("record = %+v, want the newer org/9000 policy to win", got[0])
	}
}

func TestSaveExposureKeepsOtherNames(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []ExposureRecord{
		{Name: "frigate", Port: 8212, Visibility: "org"},
		{Name: "grafana", Port: 3000, Visibility: "link"},
	} {
		if err := SaveExposure(dir, r); err != nil {
			t.Fatal(err)
		}
	}

	got, _ := LoadExposures(dir)
	if len(got) != 2 {
		t.Fatalf("loaded %d records, want 2", len(got))
	}
	// Sorted by name for a stable file.
	if got[0].Name != "frigate" || got[1].Name != "grafana" {
		t.Errorf("records not sorted by name: %+v", got)
	}
}

// A node that has never exposed anything is the common case and must not look
// like an error — otherwise every gateway start logs a spurious warning.
func TestLoadExposuresMissingFileIsNotAnError(t *testing.T) {
	got, err := LoadExposures(t.TempDir())
	if err != nil {
		t.Fatalf("missing store returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d records from an empty dir", len(got))
	}
}

// A corrupt store must be reported, not silently treated as "no exposures" —
// starting with zero routes and no message is exactly the invisible failure
// this whole change exists to remove.
func TestLoadExposuresCorruptIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, exposuresFile), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadExposures(dir); err == nil {
		t.Fatal("corrupt store parsed without error; the operator would never learn routes were lost")
	}
}

// ...but a corrupt store must not permanently wedge the node: recording a new
// exposure has to succeed by starting a fresh set.
func TestSaveExposureRecoversFromCorruptStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, exposuresFile), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SaveExposure(dir, ExposureRecord{Name: "frigate", Port: 8212, Visibility: "org"}); err != nil {
		t.Fatalf("SaveExposure over a corrupt store: %v", err)
	}
	got, err := LoadExposures(dir)
	if err != nil || len(got) != 1 || got[0].Name != "frigate" {
		t.Errorf("recovery failed: got %+v, err %v", got, err)
	}
}

func TestDeleteExposure(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []ExposureRecord{
		{Name: "frigate", Port: 8212, Visibility: "org"},
		{Name: "grafana", Port: 3000, Visibility: "org"},
	} {
		if err := SaveExposure(dir, r); err != nil {
			t.Fatal(err)
		}
	}

	if err := DeleteExposure(dir, "frigate"); err != nil {
		t.Fatalf("DeleteExposure: %v", err)
	}
	got, _ := LoadExposures(dir)
	if len(got) != 1 || got[0].Name != "grafana" {
		t.Errorf("after delete: %+v, want only grafana", got)
	}

	// Idempotent: unexposing something already gone is not an error.
	if err := DeleteExposure(dir, "frigate"); err != nil {
		t.Errorf("second DeleteExposure: %v", err)
	}
	if err := DeleteExposure(t.TempDir(), "anything"); err != nil {
		t.Errorf("DeleteExposure with no store: %v", err)
	}
}

// The store can name a private exposure's creator, so it must not be
// world-readable — same posture as the signing key it sits beside.
func TestExposureStorePermissions(t *testing.T) {
	dir := t.TempDir()
	if err := SaveExposure(dir, ExposureRecord{Name: "x", Port: 1, Visibility: "org"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, exposuresFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("exposures file mode = %o, want 600", perm)
	}
}

// A crash mid-write must not leave a truncated store that drops every exposure,
// so the write goes through a temp file + rename. Assert no stray temp file is
// left behind on the happy path.
func TestSaveExposureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveExposure(dir, ExposureRecord{Name: "x", Port: 1, Visibility: "org"}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}
