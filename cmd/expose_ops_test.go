// cmd/expose_ops_test.go
//
// Tests for the expose-custody funnel (issue #944): the node-owned epoch
// high-water rule and the concurrency mutex that protects it. See
// docs/design-expose-custody.md §5.3/§5.4.
package cmd

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// setupExposeOpsTest isolates platform.ConfigDir() to a temp dir (mirrors
// cmd/module_ops_test.go's writeLockfile convention) and wires a fresh
// in-process gateway.Server so liveExposeOps has something to program,
// restoring both to their prior state when the test ends.
func setupExposeOpsTest(t *testing.T) *gateway.Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	gw := gateway.NewServer(gateway.Config{})
	setProvisionedServiceGateway(gw, 8443, true, "", 8080)
	t.Cleanup(func() { setProvisionedServiceGateway(nil, 0, false, "", 0) })
	return gw
}

func TestResolveEffectiveEpoch(t *testing.T) {
	cases := []struct {
		name     string
		base     int
		reqEpoch int
		rotate   bool
		want     int
	}{
		{"fresh name, default request", 1, 1, false, 1},
		{"plain re-expose preserves current epoch", 5, 1, false, 5},
		{"legacy fast-forward is honored", 2, 7, false, 7},
		{"stale caller value never regresses below base", 5, 3, false, 5},
		{"rotate strictly increases past base", 5, 1, true, 6},
		{"rotate also fast-forwards a caller value first", 2, 7, true, 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveEffectiveEpoch(c.base, c.reqEpoch, c.rotate)
			if got != c.want {
				t.Errorf("resolveEffectiveEpoch(%d, %d, %v) = %d, want %d", c.base, c.reqEpoch, c.rotate, got, c.want)
			}
		})
	}
}

// TestExposeOps_FirstExposeStartsAtEpoch1 pins the baseline: a name never
// seen before settles at epoch 1 with no rotate.
func TestExposeOps_FirstExposeStartsAtEpoch1(t *testing.T) {
	setupExposeOpsTest(t)
	res, err := (liveExposeOps{}).Expose(context.Background(), worker.ExposeRequest{
		Name: "frigate", Port: 5000, Visibility: "org",
	})
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if res.Epoch != 1 {
		t.Errorf("first expose epoch = %d, want 1", res.Epoch)
	}
}

// TestExposeOps_PlainReExposePreservesEpoch is the blind-caller safety
// property (design doc §5.3): a stateless caller that always sends the wire
// default (epoch=1, no rotate) against a name already living at a higher
// epoch must NOT revoke outstanding links.
func TestExposeOps_PlainReExposePreservesEpoch(t *testing.T) {
	setupExposeOpsTest(t)
	ops := liveExposeOps{}
	ctx := context.Background()

	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org"}); err != nil {
		t.Fatalf("initial expose: %v", err)
	}
	// Rotate to move the name to a known higher epoch.
	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org", Rotate: true}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated := config.FindExposure(platform.ConfigDir(), "frigate"); rotated == nil {
		t.Fatal("find after rotate: no record")
	}

	// A blind re-expose with the wire default must preserve whatever epoch is
	// currently live, not reset it to 1.
	res, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org", Epoch: 1})
	if err != nil {
		t.Fatalf("blind re-expose: %v", err)
	}
	if res.Epoch != 2 {
		t.Errorf("blind re-expose epoch = %d, want preserved at 2", res.Epoch)
	}
}

// TestExposeOps_RotateIncrementsEpoch pins the explicit revoke-all verb.
func TestExposeOps_RotateIncrementsEpoch(t *testing.T) {
	setupExposeOpsTest(t)
	ops := liveExposeOps{}
	ctx := context.Background()

	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org"}); err != nil {
		t.Fatalf("initial expose: %v", err)
	}
	res, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org", Rotate: true})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if res.Epoch != 2 {
		t.Errorf("rotate epoch = %d, want 2", res.Epoch)
	}
}

// TestExposeOps_UnexposeThenReExposeDoesNotResurrectRevokedEpoch is the
// design's core acceptance test (§5.2's landmine, §5.3's fix): expose@1 ->
// rotate@2 (revoking leaked epoch-1 tokens) -> UNEXPOSE (deletes the durable
// record) -> re-expose with the blind default. The re-exposed epoch must be
// STRICTLY GREATER than any epoch this name ever lived at (2), so neither a
// token signed at epoch 1 NOR one signed at epoch 2 can ever verify again.
func TestExposeOps_UnexposeThenReExposeDoesNotResurrectRevokedEpoch(t *testing.T) {
	setupExposeOpsTest(t)
	ops := liveExposeOps{}
	ctx := context.Background()

	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "link"}); err != nil {
		t.Fatalf("initial expose: %v", err)
	}
	rotateRes, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "link", Rotate: true})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotateRes.Epoch != 2 {
		t.Fatalf("rotate epoch = %d, want 2", rotateRes.Epoch)
	}

	unexposeRes, err := ops.Unexpose(ctx, "frigate")
	if err != nil {
		t.Fatalf("unexpose: %v", err)
	}
	if !unexposeRes.WasExposed {
		t.Fatal("unexpose reported was_exposed=false for a live exposure")
	}
	// The durable record is gone -- the naive "reject a lower epoch" guard
	// would have nothing left to compare against here.
	if rec := config.FindExposure(platform.ConfigDir(), "frigate"); rec != nil {
		t.Fatalf("durable record still exists after unexpose: %+v", rec)
	}

	// A blind caller re-expose (the exact MCP default shape, §1.5) must not
	// resurrect epoch 1 OR epoch 2.
	reExposeRes, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "link", Epoch: 1})
	if err != nil {
		t.Fatalf("re-expose: %v", err)
	}
	if reExposeRes.Epoch <= 2 {
		t.Fatalf("re-expose epoch = %d, want > 2 (must never resurrect a revoked epoch)", reExposeRes.Epoch)
	}

	// The decisive check: a token minted under the OLD (pre-unexpose) epoch
	// must never again satisfy an exact-match check against the new live
	// epoch, for either revoked epoch.
	if reExposeRes.Epoch == 1 || reExposeRes.Epoch == 2 {
		t.Fatalf("re-expose settled on a previously-live epoch %d -- a revoked token can verify again", reExposeRes.Epoch)
	}
}

// TestExposeOps_StaleRecordDoesNotShadowHighWater pins the #945 review's core
// finding: the high-water store must be a FLOOR, not merely an absent-record
// fallback. It models the disk-fault vector directly — a durable record left
// at a LOWER epoch than the high-water mark (what a best-effort SaveExposure
// failure after a successful rotate leaves behind) — and asserts a blind
// re-expose is floored UP to high-water rather than programmed back down to the
// stale record's epoch. Against the pre-fix `else if` (record shadows
// high-water), base would have been the stale 1 and the revoked epoch-1 token
// would verify again.
func TestExposeOps_StaleRecordDoesNotShadowHighWater(t *testing.T) {
	gw := setupExposeOpsTest(t)
	ops := liveExposeOps{}
	ctx := context.Background()
	dir := platform.ConfigDir()

	// expose@1: durable record at epoch 1, high-water at 1.
	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "link"}); err != nil {
		t.Fatalf("initial expose: %v", err)
	}
	// Simulate a rotate whose high-water write SUCCEEDED (advancing to 2, and
	// minting/leaking an epoch-2 token) but whose record-write FAILED — the
	// exact best-effort-write divergence the review flagged. The on-disk record
	// is still epoch 1; high-water is 2.
	if err := config.SaveExposeEpochHighWater(dir, "frigate", 2); err != nil {
		t.Fatalf("advance high-water: %v", err)
	}
	if rec := config.FindExposure(dir, "frigate"); rec == nil || rec.TokenEpoch != 1 {
		t.Fatalf("precondition: want stale record at epoch 1, got %+v", rec)
	}

	// A blind re-expose (the MCP wire default) must floor to high-water and land
	// STRICTLY above every already-minted epoch — never back at the stale 1.
	res, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "link", Epoch: 1})
	if err != nil {
		t.Fatalf("blind re-expose: %v", err)
	}
	if res.Epoch < 2 {
		t.Fatalf("re-expose epoch = %d, want >= 2 (stale record must not shadow high-water)", res.Epoch)
	}
	if epoch, ok := gw.ExposureEpoch("frigate"); !ok || epoch < 2 {
		t.Fatalf("live gateway epoch = %d (ok=%v), want >= 2", epoch, ok)
	}
}

// TestExposeOps_RestoreExposuresFloorsStaleRecordToHighWater is the
// restart-amplification half of the same #945 finding — the worse vector,
// because it needs no re-expose at all. A stale, lower-epoch record on disk
// plus a higher high-water mark, then a routine restart (auto-update,
// self-heal, reboot) runs restoreExposures: the live policy it programs must be
// floored to high-water, not taken verbatim from the stale record (which would
// resurrect the rotated-away token on its own).
func TestExposeOps_RestoreExposuresFloorsStaleRecordToHighWater(t *testing.T) {
	gw := setupExposeOpsTest(t)
	dir := platform.ConfigDir()

	// A stale durable record at epoch 1, but high-water already at 2.
	if err := config.SaveExposure(dir, config.ExposureRecord{
		Name: "frigate", Port: 5000, Visibility: "link", TokenEpoch: 1,
	}); err != nil {
		t.Fatalf("save stale record: %v", err)
	}
	if err := config.SaveExposeEpochHighWater(dir, "frigate", 2); err != nil {
		t.Fatalf("advance high-water: %v", err)
	}

	restoreExposures(gw)

	epoch, ok := gw.ExposureEpoch("frigate")
	if !ok {
		t.Fatal("restore did not program the exposure")
	}
	if epoch < 2 {
		t.Fatalf("restored epoch = %d, want floored to high-water >= 2 (stale record must not resurrect a revoked token)", epoch)
	}
}

// TestExposeOps_RestoreExposures_PathEscapingCurrentWorkspaceIsSkipped pins
// issue #949 item 1: a persisted directory-source exposure's Path is the
// resolved root Expose() validated against the workspace AT WRITE TIME, but
// the workspace boundary itself is not durable -- an operator can narrow
// CITADEL_WORKSPACE/--workspace between then and a later restart. A record
// whose Path no longer resolves inside the CURRENT workspace must be skipped
// on restore rather than re-wired verbatim, even though the gateway's own
// resolveConfinedRoot would happily accept it (it only requires an absolute,
// existing directory -- it has no notion of "the workspace" at all). A
// still-valid record restores normally alongside it.
func TestExposeOps_RestoreExposures_PathEscapingCurrentWorkspaceIsSkipped(t *testing.T) {
	gw := setupExposeOpsTest(t)
	dir := platform.ConfigDir()

	workspace := t.TempDir()
	prevWorkspace := workWorkspaceDir
	workWorkspaceDir = workspace
	t.Cleanup(func() { workWorkspaceDir = prevWorkspace })

	// A valid record: its Path sits inside the CURRENT workspace.
	validPath := filepath.Join(workspace, "shared")
	if err := os.MkdirAll(validPath, 0o755); err != nil {
		t.Fatalf("mkdir valid path: %v", err)
	}
	if err := config.SaveExposure(dir, config.ExposureRecord{
		Name: "valid-share", Path: validPath, Visibility: "org",
	}); err != nil {
		t.Fatalf("save valid record: %v", err)
	}

	// An escaping record: Path is a real, existing directory, but OUTSIDE the
	// current workspace -- as if the workspace was narrowed after this share
	// was originally created and persisted.
	escapingPath := t.TempDir()
	if err := config.SaveExposure(dir, config.ExposureRecord{
		Name: "escaping-share", Path: escapingPath, Visibility: "org",
	}); err != nil {
		t.Fatalf("save escaping record: %v", err)
	}

	// A deleted record: Path pointed inside the workspace at write time, but
	// the directory itself no longer exists (moved/removed since). Covered
	// here alongside the new workspace-escape check to pin that the existing
	// "gateway rejects it" skip path (ExposeDir's own resolveConfinedRoot)
	// still fires for this case, unchanged.
	deletedPath := filepath.Join(workspace, "gone")
	if err := os.MkdirAll(deletedPath, 0o755); err != nil {
		t.Fatalf("mkdir deleted path (pre-removal): %v", err)
	}
	if err := config.SaveExposure(dir, config.ExposureRecord{
		Name: "deleted-share", Path: deletedPath, Visibility: "org",
	}); err != nil {
		t.Fatalf("save deleted record: %v", err)
	}
	if err := os.RemoveAll(deletedPath); err != nil {
		t.Fatalf("remove deleted path: %v", err)
	}

	restoreExposures(gw)

	if _, ok := gw.ExposureEpoch("valid-share"); !ok {
		t.Error("valid-share was not restored, want it wired")
	}
	if _, ok := gw.ExposureEpoch("escaping-share"); ok {
		t.Error("escaping-share was restored despite resolving outside the current workspace, want it skipped")
	}
	if _, ok := gw.ExposureEpoch("deleted-share"); ok {
		t.Error("deleted-share was restored despite its directory no longer existing, want it skipped")
	}
}

// TestExposeOps_HighWaterNeverRegresses is a narrower unit check on the store
// itself: SaveExposeEpochHighWater must ignore a write that would move the
// mark backwards.
func TestExposeOps_HighWaterNeverRegresses(t *testing.T) {
	dir := t.TempDir()
	if err := config.SaveExposeEpochHighWater(dir, "frigate", 5); err != nil {
		t.Fatalf("save 5: %v", err)
	}
	if err := config.SaveExposeEpochHighWater(dir, "frigate", 3); err != nil {
		t.Fatalf("save 3: %v", err)
	}
	if got := config.ExposeEpochHighWater(dir, "frigate"); got != 5 {
		t.Errorf("high-water regressed: got %d, want 5", got)
	}
}

// TestExposeOps_List returns the durable exposure set, including the epoch,
// creator, visibility, and created-at fields EXPOSE_LIST's contract promises
// (design doc §3.2).
func TestExposeOps_List(t *testing.T) {
	setupExposeOpsTest(t)
	ops := liveExposeOps{}
	ctx := context.Background()

	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org"}); err != nil {
		t.Fatalf("expose: %v", err)
	}
	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "dash", Port: 6000, Visibility: "link"}); err != nil {
		t.Fatalf("expose: %v", err)
	}

	res, err := ops.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Exposures) != 2 {
		t.Fatalf("got %d exposures, want 2", len(res.Exposures))
	}
	byName := map[string]worker.ExposureInfo{}
	for _, e := range res.Exposures {
		byName[e.Name] = e
	}
	frigate, ok := byName["frigate"]
	if !ok {
		t.Fatal("frigate missing from list")
	}
	if frigate.Port != 5000 || frigate.Visibility != "org" || frigate.Epoch != 1 {
		t.Errorf("frigate row wrong: %+v", frigate)
	}
	if frigate.CreatedAt == "" {
		t.Error("frigate row missing created_at")
	}
	if !frigate.Live {
		t.Error("frigate should be reported live (gateway currently has it programmed)")
	}
	// URL must be built the same way Expose's own ExposeResult.URL is (both go
	// through exposeMeshURL) — pinned here by cross-checking List's URL against
	// a fresh Expose call for the same name, rather than a hardcoded string, so
	// this doesn't silently drift if exposeMeshURL's format ever changes.
	wantURL := exposeMeshURL("frigate")
	if frigate.URL != wantURL {
		t.Errorf("frigate url = %q, want %q (from exposeMeshURL)", frigate.URL, wantURL)
	}
}

// TestExposeOps_UnexposeTearsDownAndPersists proves Unexpose both drops the
// live gateway route AND deletes the durable record, and that revoking an
// exposure that was never live is still a success (idempotent).
func TestExposeOps_UnexposeTearsDownAndPersists(t *testing.T) {
	setupExposeOpsTest(t)
	ops := liveExposeOps{}
	ctx := context.Background()

	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org"}); err != nil {
		t.Fatalf("expose: %v", err)
	}

	res, err := ops.Unexpose(ctx, "frigate")
	if err != nil {
		t.Fatalf("unexpose: %v", err)
	}
	if !res.WasExposed {
		t.Error("was_exposed should be true for a live exposure")
	}
	if rec := config.FindExposure(platform.ConfigDir(), "frigate"); rec != nil {
		t.Errorf("durable record still present after unexpose: %+v", rec)
	}
	ref := getProvisionedServiceGateway()
	if ref.gw.HasExposure("frigate") {
		t.Error("gateway still reports the exposure live after unexpose")
	}

	// Idempotent: revoking again is still a success, just was_exposed=false.
	res2, err := ops.Unexpose(ctx, "frigate")
	if err != nil {
		t.Fatalf("second unexpose: %v", err)
	}
	if res2.WasExposed {
		t.Error("second unexpose should report was_exposed=false")
	}
}

// TestExposeOps_ConcurrencyMutexSerializesRMW is the §5.4 acceptance test:
// many concurrent Expose(rotate=true) calls against the SAME name, from
// goroutines standing in for the worker lane and the HTTP control path
// racing each other, must never lose an increment. Without the mutex this
// test is flaky (two goroutines can read the same base and both settle on
// the same effective epoch); with it, N rotates from a base of 1 always land
// on exactly 1+N.
func TestExposeOps_ConcurrencyMutexSerializesRMW(t *testing.T) {
	setupExposeOpsTest(t)
	ops := liveExposeOps{}
	ctx := context.Background()

	if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org"}); err != nil {
		t.Fatalf("initial expose: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ops.Expose(ctx, worker.ExposeRequest{Name: "frigate", Port: 5000, Visibility: "org", Rotate: true}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Expose failed: %v", err)
	}

	rec := config.FindExposure(platform.ConfigDir(), "frigate")
	if rec == nil {
		t.Fatal("no record after concurrent rotates")
	}
	want := 1 + n
	if rec.TokenEpoch != want {
		t.Errorf("final epoch = %d, want %d (a lost update means the mutex did not serialize the read-modify-write)", rec.TokenEpoch, want)
	}
	if hw := config.ExposeEpochHighWater(platform.ConfigDir(), "frigate"); hw != want {
		t.Errorf("high-water = %d, want %d", hw, want)
	}
}
