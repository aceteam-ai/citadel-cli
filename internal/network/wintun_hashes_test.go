// internal/network/wintun_hashes_test.go
package network

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"testing"
)

// TestWintunAssetHashesMatchPins guards against the one failure mode that
// matters most for a redistributed, license-restricted binary: the checked-in
// wintun.dll bytes silently drifting from what was reviewed and pinned (a bad
// merge, a wrong-arch copy, manual "fix" of the binary, etc). It reads the
// asset files directly off disk rather than through go:embed so it runs on
// every platform's `go test ./...`, not just a Windows one this repo's CI
// does not have.
func TestWintunAssetHashesMatchPins(t *testing.T) {
	if len(wintunAssetPaths) == 0 {
		t.Fatal("wintunAssetPaths is empty")
	}

	// Deterministic order for stable test output.
	arches := make([]string, 0, len(wintunAssetPaths))
	for arch := range wintunAssetPaths {
		arches = append(arches, arch)
	}
	sort.Strings(arches)

	for _, arch := range arches {
		path := wintunAssetPaths[arch]
		wantHash, ok := wintunHashPins[arch]
		if !ok {
			t.Errorf("%s: no pinned hash for arch %q", path, arch)
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s: embedded driver is empty", path)
			continue
		}

		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != wantHash {
			t.Errorf("%s: sha256 = %s, want %s (pin in wintun_hashes.go)", path, got, wantHash)
		}
	}
}

// TestWintunAssetPathsAndPinsAgree catches the case where an arch is added to
// one map and not the other -- silent on its own (the loader would just fail
// to find a hash at runtime), so pin it here.
func TestWintunAssetPathsAndPinsAgree(t *testing.T) {
	for arch := range wintunAssetPaths {
		if _, ok := wintunHashPins[arch]; !ok {
			t.Errorf("arch %q has an asset path but no pinned hash", arch)
		}
	}
	for arch := range wintunHashPins {
		if _, ok := wintunAssetPaths[arch]; !ok {
			t.Errorf("arch %q has a pinned hash but no asset path", arch)
		}
	}
}
