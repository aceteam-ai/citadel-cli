package devicemode

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func fakeStat(existing ...string) func(string) (os.FileInfo, error) {
	set := map[string]bool{}
	for _, p := range existing {
		set[p] = true
	}
	return func(path string) (os.FileInfo, error) {
		if set[path] {
			return nil, nil //nolint:nilnil // only existence is checked
		}
		return nil, os.ErrNotExist
	}
}

func lookPathHit(path string) func(string) (string, error) {
	return func(string) (string, error) { return path, nil }
}

func lookPathMiss(string) (string, error) {
	return "", fmt.Errorf("not found")
}

func TestFindTailscaleEnvOverride(t *testing.T) {
	got, err := findTailscale("linux", lookPathMiss, fakeStat("/opt/ts/tailscale"), "/opt/ts/tailscale")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/ts/tailscale" {
		t.Fatalf("got %q", got)
	}
}

func TestFindTailscaleEnvOverrideMissingFails(t *testing.T) {
	// A broken explicit override must error, not silently fall back.
	if _, err := findTailscale("linux", lookPathHit("/usr/bin/tailscale"), fakeStat(), "/nope"); err == nil {
		t.Fatal("expected error for missing override")
	}
}

func TestFindTailscalePath(t *testing.T) {
	got, err := findTailscale("linux", lookPathHit("/usr/bin/tailscale"), fakeStat(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/bin/tailscale" {
		t.Fatalf("got %q", got)
	}
}

func TestFindTailscaleMacAppStoreFallback(t *testing.T) {
	got, err := findTailscale("darwin", lookPathMiss, fakeStat(MacAppStoreTailscale), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != MacAppStoreTailscale {
		t.Fatalf("got %q", got)
	}
}

func TestFindTailscaleNotFound(t *testing.T) {
	if _, err := findTailscale("linux", lookPathMiss, fakeStat(), ""); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestParseStatusJSON(t *testing.T) {
	out := []byte(`{
		"BackendState": "Running",
		"Self": {"KeyExpiry": "2026-12-01T00:00:00Z"}
	}`)
	st, err := ParseStatusJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.BackendState != "Running" {
		t.Fatalf("BackendState=%q", st.BackendState)
	}
	want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if st.Self.KeyExpiry == nil || !st.Self.KeyExpiry.Equal(want) {
		t.Fatalf("KeyExpiry=%v", st.Self.KeyExpiry)
	}
}

func TestParseStatusJSONNoExpiry(t *testing.T) {
	st, err := ParseStatusJSON([]byte(`{"BackendState": "NeedsLogin", "Self": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	if st.BackendState != "NeedsLogin" {
		t.Fatalf("BackendState=%q", st.BackendState)
	}
	if st.Self.KeyExpiry != nil {
		t.Fatalf("expected nil KeyExpiry, got %v", st.Self.KeyExpiry)
	}
}

func TestParseStatusJSONRejectsGarbage(t *testing.T) {
	if _, err := ParseStatusJSON([]byte(`not json`)); err == nil {
		t.Fatal("expected parse error")
	}
	if _, err := ParseStatusJSON([]byte(`{}`)); err == nil {
		t.Fatal("expected missing-BackendState error")
	}
}
