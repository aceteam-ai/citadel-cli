// internal/status/swap_preflight_test.go
//
// Unit tests for citadel-cli#683's honest warm-on-demand preflight. None of
// these rely on a real docker/podman daemon or a real ~/citadel-cache: every
// I/O seam (engineImagePresentFn, engineWeightsPresentFn,
// citadelCacheBaseDirFn) is a package var overridden here, mirroring the
// existing runningContainerNames injection pattern.
package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// --- diskHeadroomBlocked (pure function) ---------------------------------

func TestDiskHeadroomBlocked_NoSignalNeverBlocks(t *testing.T) {
	// DiskTotalGB<=0 means the collector never read disk metrics at all -- an
	// ABSENT signal, not a confirmed shortfall, so it must never block.
	if diskHeadroomBlocked(SystemMetrics{}) {
		t.Errorf("expected no block on a zero-value (absent) SystemMetrics")
	}
	if diskHeadroomBlocked(SystemMetrics{DiskAvailableGB: 0.001, DiskPercent: 99.9, DiskTotalGB: 0}) {
		t.Errorf("expected no block when DiskTotalGB<=0 even with alarming other fields")
	}
}

func TestDiskHeadroomBlocked_LowFreeBytesBlocks(t *testing.T) {
	sys := SystemMetrics{DiskTotalGB: 500, DiskAvailableGB: 2, DiskPercent: 60}
	if !diskHeadroomBlocked(sys) {
		t.Errorf("expected block: free space (%vGB) is below the %vGB floor", sys.DiskAvailableGB, diskPressureMinFreeGB)
	}
}

func TestDiskHeadroomBlocked_HighPercentBlocks(t *testing.T) {
	// Plenty of free bytes in absolute terms, but the percentage signal alone
	// must still block per the issue's explicit "disk_percent below threshold"
	// clause.
	sys := SystemMetrics{DiskTotalGB: 2000, DiskAvailableGB: 40, DiskPercent: 95}
	if !diskHeadroomBlocked(sys) {
		t.Errorf("expected block: disk_percent (%v) is at/above the %v threshold", sys.DiskPercent, diskPressurePercentThreshold)
	}
}

func TestDiskHeadroomBlocked_HealthyDiskDoesNotBlock(t *testing.T) {
	sys := SystemMetrics{DiskTotalGB: 500, DiskAvailableGB: 100, DiskPercent: 50}
	if diskHeadroomBlocked(sys) {
		t.Errorf("expected no block on a healthy disk")
	}
}

// --- engineImageRef / defaultEngineImagePresent ---------------------------

// Every current services.ServiceMap entry (including build-based bonsai)
// declares a fixed `image:` tag with no env-var interpolation -- the
// property defaultEngineImagePresent's doc comment relies on. If a future
// engine's compose drops this, this test catches it (and that engine would
// silently fail open on the image-present check, per engineImageRef's
// documented behavior).
func TestEngineImageRefResolvesForEveryServiceMapEntry(t *testing.T) {
	for name := range services.ServiceMap {
		if ref := engineImageRef(name); ref == "" {
			t.Errorf("engineImageRef(%q) = \"\", want a non-empty image tag", name)
		}
	}
}

func TestEngineImageRef_UnknownEngineReturnsEmpty(t *testing.T) {
	if ref := engineImageRef("no-such-engine"); ref != "" {
		t.Errorf("engineImageRef(unknown) = %q, want empty", ref)
	}
}

func TestDefaultEngineImagePresent_NoRefFailsOpen(t *testing.T) {
	// An engine with no resolvable image ref carries no signal -- the check
	// must never manufacture a block from nothing.
	if !defaultEngineImagePresent("no-such-engine") {
		t.Errorf("expected fail-open (true) for an engine with no image ref")
	}
}

// --- defaultEngineWeightsPresent / citadelCacheBaseDirFn ------------------

func TestDefaultEngineWeightsPresent_EmptyDirIsAbsent(t *testing.T) {
	dir := t.TempDir()
	origBase := citadelCacheBaseDirFn
	citadelCacheBaseDirFn = func() string { return dir }
	t.Cleanup(func() { citadelCacheBaseDirFn = origBase })

	// bonsai's cache dir under the temp base exists but is empty.
	cache := services.EngineCacheDirs["bonsai"]
	if err := os.MkdirAll(filepath.Join(dir, cache.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if defaultEngineWeightsPresent("bonsai") {
		t.Errorf("expected weights NOT present in an empty cache directory")
	}
}

func TestDefaultEngineWeightsPresent_NonEmptyDirIsPresent(t *testing.T) {
	dir := t.TempDir()
	origBase := citadelCacheBaseDirFn
	citadelCacheBaseDirFn = func() string { return dir }
	t.Cleanup(func() { citadelCacheBaseDirFn = origBase })

	cache := services.EngineCacheDirs["bonsai"]
	weightsDir := filepath.Join(dir, cache.Dir)
	if err := os.MkdirAll(weightsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(weightsDir, "Bonsai-27B-Q1_0.gguf"), []byte("fake-weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !defaultEngineWeightsPresent("bonsai") {
		t.Errorf("expected weights present when the cache dir has a non-empty file")
	}
}

func TestDefaultEngineWeightsPresent_MissingDirIsAbsent(t *testing.T) {
	dir := t.TempDir() // exists, but no engine subdirectory ever created
	origBase := citadelCacheBaseDirFn
	citadelCacheBaseDirFn = func() string { return dir }
	t.Cleanup(func() { citadelCacheBaseDirFn = origBase })

	if defaultEngineWeightsPresent("bonsai") {
		t.Errorf("expected weights NOT present when the cache dir was never created")
	}
}

func TestDefaultEngineWeightsPresent_UnknownEngineFailsOpen(t *testing.T) {
	if !defaultEngineWeightsPresent("no-such-engine") {
		t.Errorf("expected fail-open (true) for an engine with no EngineCacheDirs entry")
	}
}

// --- collectInstalledEngines integration: the three new clauses -----------

// TestCollectInstalledEngines_ImageMissingBlocksSwap pins the exact incident
// citadel-cli#683 describes: the compose YAML survived (writeInstalledEngine
// materializes it) but the image was GC'd. The entry must still be reported
// (so the platform can render why), but must NOT read as a fast warm-on-
// demand candidate -- SwapBlocked=true, reason "image_missing", no VRAM
// estimate attached.
func TestCollectInstalledEngines_ImageMissingBlocksSwap(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "")

	origImg, origWeights := engineImagePresentFn, engineWeightsPresentFn
	engineImagePresentFn = func(string) bool { return false } // "GC'd"
	engineWeightsPresentFn = func(string) bool { return true }
	t.Cleanup(func() {
		engineImagePresentFn = origImg
		engineWeightsPresentFn = origWeights
	})

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	got := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{})

	var bonsai *ServiceInfo
	for i := range got {
		if got[i].Name == "bonsai" {
			bonsai = &got[i]
		}
	}
	if bonsai == nil {
		t.Fatalf("expected bonsai still reported despite the missing image, got %+v", got)
	}
	if !bonsai.SwapBlocked {
		t.Errorf("SwapBlocked = false, want true")
	}
	if bonsai.SwapBlockedReason != "image_missing" {
		t.Errorf("SwapBlockedReason = %q, want %q", bonsai.SwapBlockedReason, "image_missing")
	}
	if bonsai.VRAMEstimateMB != 0 {
		t.Errorf("VRAMEstimateMB = %d, want 0 (no confidently-wrong ETA number for a blocked engine)", bonsai.VRAMEstimateMB)
	}
	// Status/Resident are unaffected -- still an honest "installed, not
	// running" statement, just no longer advertised as swappable.
	if bonsai.Status != ServiceStatusStopped {
		t.Errorf("Status = %q, want stopped", bonsai.Status)
	}
	if bonsai.Resident == nil || *bonsai.Resident {
		t.Errorf("Resident = %v, want non-nil false", bonsai.Resident)
	}
}

func TestCollectInstalledEngines_WeightsMissingBlocksSwap(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "")

	origImg, origWeights := engineImagePresentFn, engineWeightsPresentFn
	engineImagePresentFn = func(string) bool { return true }
	engineWeightsPresentFn = func(string) bool { return false } // cache swept
	t.Cleanup(func() {
		engineImagePresentFn = origImg
		engineWeightsPresentFn = origWeights
	})

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	got := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{})

	var bonsai *ServiceInfo
	for i := range got {
		if got[i].Name == "bonsai" {
			bonsai = &got[i]
		}
	}
	if bonsai == nil {
		t.Fatalf("expected bonsai still reported despite the missing weights, got %+v", got)
	}
	if !bonsai.SwapBlocked {
		t.Errorf("SwapBlocked = false, want true")
	}
	if bonsai.SwapBlockedReason != "weights_missing" {
		t.Errorf("SwapBlockedReason = %q, want %q", bonsai.SwapBlockedReason, "weights_missing")
	}
	if bonsai.VRAMEstimateMB != 0 {
		t.Errorf("VRAMEstimateMB = %d, want 0", bonsai.VRAMEstimateMB)
	}
}

// TestCollectInstalledEngines_DiskPressureBlocksSwap pins the clause the
// issue calls out as the one "a well-resourced box never exercises and a
// consumer box exercises constantly": image and weights are BOTH present,
// but the node is nearly full.
func TestCollectInstalledEngines_DiskPressureBlocksSwap(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "")
	stubHotswapPreflightPass(t) // image/weights both pass; only disk blocks

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	sys := SystemMetrics{DiskTotalGB: 500, DiskAvailableGB: 2, DiskPercent: 99}
	got := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, sys)

	var bonsai *ServiceInfo
	for i := range got {
		if got[i].Name == "bonsai" {
			bonsai = &got[i]
		}
	}
	if bonsai == nil {
		t.Fatalf("expected bonsai still reported despite disk pressure, got %+v", got)
	}
	if !bonsai.SwapBlocked {
		t.Errorf("SwapBlocked = false, want true")
	}
	if bonsai.SwapBlockedReason != "disk_pressure" {
		t.Errorf("SwapBlockedReason = %q, want %q", bonsai.SwapBlockedReason, "disk_pressure")
	}
	if bonsai.VRAMEstimateMB != 0 {
		t.Errorf("VRAMEstimateMB = %d, want 0", bonsai.VRAMEstimateMB)
	}
}

// TestCollectInstalledEngines_AllChecksPassAdvertisedAsBefore is the explicit
// happy-path pin the issue's fix demands: when every clause passes, the
// entry is advertised exactly as it was pre-#683 -- crucially, the NEW
// SwapBlocked/SwapBlockedReason fields must be entirely ABSENT from the
// marshaled JSON, not merely false/empty, so a platform that doesn't know
// about them yet sees a byte-identical payload.
func TestCollectInstalledEngines_AllChecksPassAdvertisedAsBefore(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "")
	stubHotswapPreflightPass(t)

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	sys := SystemMetrics{DiskTotalGB: 500, DiskAvailableGB: 100, DiskPercent: 40}
	got := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, sys)

	if len(got) != 1 {
		t.Fatalf("expected exactly one advertised engine, got %d: %+v", len(got), got)
	}
	bonsai := got[0]
	if bonsai.SwapBlocked {
		t.Errorf("SwapBlocked = true, want false")
	}
	if bonsai.SwapBlockedReason != "" {
		t.Errorf("SwapBlockedReason = %q, want empty", bonsai.SwapBlockedReason)
	}
	if want := engineVRAMEstimateMB["bonsai"]; bonsai.VRAMEstimateMB != want {
		t.Errorf("VRAMEstimateMB = %d, want the table estimate %d", bonsai.VRAMEstimateMB, want)
	}

	b, err := json.Marshal(bonsai)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "swap_blocked") {
		t.Errorf("flag-off-equivalent JSON must not contain a swap_blocked key: %s", s)
	}
}
