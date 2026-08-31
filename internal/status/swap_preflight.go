// internal/status/swap_preflight.go
//
// Honest warm-on-demand preflight for collectInstalledEngines (citadel-cli#683).
//
// Before this file, an engine was advertised as an installed/swappable model
// purely because its compose YAML existed on disk (engineComposeMaterialized,
// an os.Stat) and a model id resolved. That is a claim about a file, not about
// serveability: it survives exactly the event that makes it false. On a
// disk-pressured node (the normal condition for the RTX-3090-class hardware
// citadel targets, not a fault state) Docker can GC an engine's image while
// the compose YAML and citadel.yaml entry remain untouched, or an operator's
// HF cache directory can be swept, leaving the node advertising a model it
// can no longer load without a multi-GB pull that may exceed the swap budget
// (swapBackgroundMaxDur, swap.go) or die on "no space left on device".
//
// Three additional preflight checks close that gap: image present, weights
// present, disk headroom. Each is independently injectable (package vars,
// mirroring runningContainerNames' shape in running_services.go) so tests
// never need a real docker daemon or a real ~/citadel-cache.
//
// Deliberately NOT duplicated here: auto-start-declared and model-id-resolves
// (the latter already gates collectInstalledEngines via resolveInstalledModel
// returning "") and VRAM-fits-after-eviction (internal/status/preempt.go's
// PlanPreemption, used at deploy time -- wiring it into the heartbeat
// advertisement itself is a larger, separate change, not part of this fix).
package status

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/shirou/gopsutil/v3/disk"
	"gopkg.in/yaml.v3"
)

// diskPressureMinFreeGB is the free-space floor below which the node refuses
// to advertise an installed-but-stopped engine as a fast warm-on-demand
// candidate, even when its image and weights are already present: a start can
// still need to write (a floating `:latest` tag re-pull, a partial/resumed
// weights directory), and the smallest embedded engine's weights alone
// (bonsai's ~3.8GB GGUF) already exceed a token margin. There is no specific
// pull-size estimate available here (unlike a real MODEL_CACHE_PULL, which
// knows the exact repo/file it is about to fetch), so this is a generous,
// engine-agnostic floor rather than internal/jobs/disk_space.go's #828
// required-size-plus-margin arithmetic.
const diskPressureMinFreeGB = 5.0

// diskPressurePercentThreshold: disk usage at/above this percent is "nearly
// full" and refuses the honest advertisement outright even when
// DiskAvailableGB alone looks adequate -- a percent-based signal catches a
// filesystem in a bad state a raw free-byte count can miss, and matches the
// disk_percent gate the citadel-cli#683 issue calls for explicitly.
const diskPressurePercentThreshold = 90.0

// diskHeadroomBlocked reports whether the node's disk state should block
// honest warm-on-demand advertisement. sys.DiskTotalGB<=0 means the collector
// could not read disk metrics at all (no GPU/disk probe, or a probe failure)
// -- that is an ABSENT signal, not a confirmed shortfall, so it never blocks
// (the same fail-open convention status.PlanRAMPreflight and
// internal/jobs.planDiskPreflight already use for an absent requirement).
func diskHeadroomBlocked(sys SystemMetrics) bool {
	if sys.DiskTotalGB <= 0 {
		return false
	}
	if sys.DiskAvailableGB < diskPressureMinFreeGB {
		return true
	}
	if sys.DiskPercent >= diskPressurePercentThreshold {
		return true
	}
	return false
}

// EngineServeablePreflight is the exported entry point citadel-cli#956 adds so
// the node's OWN on-demand swap path (internal/worker/swap.go's
// SwapManager.EnsureResident) can fail fast on a genuinely unserveable engine
// before attempting a doomed multi-GB pull -- defense-in-depth alongside the
// heartbeat honesty check above (collectInstalledEngines), since the image can
// be GC'd in the race between a heartbeat and a dispatch. It is a thin
// composition of the SAME three checks collectInstalledEngines already uses;
// it does not reimplement or weaken any of their fail-open behavior.
//
// Ordering is cheap-first: engineWeightsPresentFn and diskHeadroomBlocked are
// pure local reads (a directory walk, a pre-collected SystemMetrics field),
// while engineImagePresentFn shells out to `docker image inspect` (bounded by
// imageInspectTimeout, but still the only check that can be slow) -- so a
// swap that would fail on weights or disk never pays that cost.
//
// Returns blocked=true with a machine-readable reason
// ("weights_missing" | "disk_pressure" | "image_missing") ONLY on a
// positively-classified genuine absence. Returns blocked=false, "" both when
// the engine is serveable AND when a check could not determine the answer
// (couldn't reach docker, disk metrics never collected, ...) -- fail OPEN,
// the same direction every check below is individually documented to take. A
// false block here fails a healthy inference request, which is worse than the
// doomed pull this preflight exists to prevent.
func EngineServeablePreflight(name string, sys SystemMetrics) (blocked bool, reason string) {
	if !engineWeightsPresentFn(name) {
		return true, "weights_missing"
	}
	if diskHeadroomBlocked(sys) {
		return true, "disk_pressure"
	}
	if !engineImagePresentFn(name) {
		return true, "image_missing"
	}
	return false, ""
}

// DiskMetricsOnly reads just the root filesystem's disk usage into a
// SystemMetrics, leaving every other field zero. It is the lightweight
// counterpart to Collector.collectSystemMetrics (memory + a 100ms CPU sample +
// disk) for a caller that only needs diskHeadroomBlocked's inputs and cannot
// afford a full heartbeat-shaped collection on every call --
// SwapManager.EnsureResident (internal/worker/swap.go) runs
// EngineServeablePreflight on every on-demand swap decision, not once per
// ~30s heartbeat tick like collectInstalledEngines above.
//
// Reads the same "/" mount via the same gopsutil call collectSystemMetrics
// uses, so the two never disagree about which filesystem is being measured.
// Returns a zero SystemMetrics (DiskTotalGB<=0) on a read failure --
// diskHeadroomBlocked's own fail-open contract for an absent signal takes it
// from there; this function does not need its own fallback logic.
//
// A plain function, not a package var: unlike engineImagePresentFn/
// engineWeightsPresentFn above, nothing in THIS package calls it internally
// (EngineServeablePreflight takes sys as a parameter), so there is no
// in-package seam that needs stubbing. Callers that need to stub it (the
// SwapManager wiring in internal/worker) inject it through their OWN
// package-private field instead.
func DiskMetricsOnly() SystemMetrics {
	var metrics SystemMetrics
	if d, err := disk.Usage("/"); err == nil {
		metrics.DiskUsedGB = float64(d.Used) / (1024 * 1024 * 1024)
		metrics.DiskTotalGB = float64(d.Total) / (1024 * 1024 * 1024)
		metrics.DiskPercent = d.UsedPercent
		metrics.DiskAvailableGB = float64(d.Free) / (1024 * 1024 * 1024)
	}
	return metrics
}

// engineImagePresentFn checks whether an engine's compose-declared image
// exists in the local container image store. A package var (like
// runningContainerNames in running_services.go) so tests fake it directly
// rather than requiring a real docker/podman daemon. Overridden in tests via
// t.Cleanup-guarded reassignment.
var engineImagePresentFn = defaultEngineImagePresent

// defaultEngineImagePresent is engineImagePresentFn's production wiring: a
// real `<engine> image inspect <ref>`, NOT os.Stat on the compose YAML. This
// is what actually distinguishes "docker GC'd the image, YAML survived" from
// "genuinely never pulled" -- the citadel-cli#683 incident. Works uniformly
// for a build-based engine (bonsai) too: its compose declares BOTH `build:`
// and a fixed `image: citadel-bonsai:local` (the tag compose assigns to the
// built image), so the same inspect call answers "was it ever built" exactly
// as it answers "was it ever pulled" for a prebuilt-image engine.
func defaultEngineImagePresent(name string) bool {
	ref := engineImageRef(name)
	if ref == "" {
		// No fixed image ref to check against -- an engine not in ServiceMap,
		// or a compose file with no `image:` line at all. No signal, so this
		// clause never blocks on its own (fail open, matching the disk clause).
		return true
	}
	engineBin := catalog.SelectContainerRuntime().EngineBin
	if engineBin == "" {
		engineBin = "docker"
	}
	return runImageInspect(engineBin, ref)
}

// imageInspectTimeout bounds the docker/podman `image inspect` subprocess.
// collectInstalledEngines runs synchronously inside Collect() (the whole
// heartbeat collection), so an unbounded call here would let a
// wedged/unreachable daemon hang the ENTIRE heartbeat, not just this one
// field -- caught in review of citadel-cli#683's first pass. A package var
// (not a const) so a test can shrink it to force the timeout path without an
// actual multi-second sleep. Mirrors the bounded-docker-call pattern already
// used for footprint collection (footprint.go's footprintCollectTimeout).
var imageInspectTimeout = 5 * time.Second

// runImageInspect runs `<engineBin> image inspect <ref>` and classifies the
// result. A package var (like engineImagePresentFn/engineWeightsPresentFn
// above) so tests can point engineBin at a fake command and drive the
// error-handling branches directly, without a real docker/podman daemon.
//
// The classification is the fix for the citadel-cli#683 review finding: a
// naive `err == nil` check treats EVERY failure -- genuine "no such image",
// AND daemon-unreachable/restarting, missing binary, permission-denied, or a
// timeout -- as "image absent". A routine `systemctl restart docker` at
// heartbeat time would then make every installed-but-stopped engine
// node-wide flip to image_missing even though every image is present,
// exactly the "confidently wrong signal" #683 itself warns against, just
// moved from "file exists" to "docker call succeeded". Only a command that
// ran to completion AND whose stderr clearly reports a genuine miss counts as
// absent; everything else fails OPEN (does not block) -- when in doubt, a
// false "swappable" is the pre-existing, already-accepted risk, while a false
// "blocked" is a NEW regression this check must not introduce.
var runImageInspect = defaultRunImageInspect

func defaultRunImageInspect(engineBin, ref string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), imageInspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, engineBin, "image", "inspect", ref)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true // present
	}
	if ctx.Err() == context.DeadlineExceeded {
		return true // couldn't determine in time -- fail open, not blocked
	}
	if imageGenuinelyMissing(stderr.String()) {
		return false // confirmed absent
	}
	// *exec.Error (binary not found), a daemon-connection error, permission
	// denied, or any output we can't positively classify as a genuine miss --
	// couldn't determine, so fail open.
	return true
}

// imageGenuinelyMissing reports whether stderr output from `image inspect`
// clearly indicates the image is confirmed absent, as opposed to some other
// failure (daemon unreachable, permission denied, ...). docker emits
// "No such image", podman emits "no such image" / "image not known".
// Deliberately conservative: an unrecognized message is NOT treated as a
// genuine miss (see runImageInspect's doc comment for why that direction is
// load-bearing).
func imageGenuinelyMissing(stderrOutput string) bool {
	s := strings.ToLower(stderrOutput)
	return strings.Contains(s, "no such image") || strings.Contains(s, "image not known")
}

// engineImageRef returns the `image:` value declared in an engine's embedded
// compose file (services.ServiceMap[name]), or "" if the engine or the field
// is absent. Every current ServiceMap entry, including build-based bonsai,
// declares a fixed image tag with no env-var interpolation, so this is a
// compile-time-embedded, static lookup -- no on-node file read.
func engineImageRef(name string) string {
	compose, ok := services.ServiceMap[name]
	if !ok {
		return ""
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return ""
	}
	for _, svc := range doc.Services {
		if svc.Image != "" {
			return svc.Image
		}
	}
	return ""
}

// engineWeightsPresentFn checks whether an engine's canonical cache directory
// (services.EngineCacheDirs, citadel-cli#682/#906) actually holds weights on
// disk. A package var for the same testability reason as
// engineImagePresentFn above.
var engineWeightsPresentFn = defaultEngineWeightsPresent

// defaultEngineWeightsPresent is engineWeightsPresentFn's production wiring.
// "Holds them" is deliberately coarse for v1 (dir exists and is non-empty),
// per citadel-cli#683: over-engineering a per-file manifest check here is out
// of scope, and for engines sharing the HF-hub cache directory (vllm, sglang,
// diffusers, ...) this can under-detect a missing model for one specific
// engine when ANOTHER hf-hub engine's weights are present in the same shared
// directory -- a known, accepted v1 approximation, not a correctness bug this
// change claims to close.
func defaultEngineWeightsPresent(name string) bool {
	cache, ok := services.EngineCacheDirs[name]
	if !ok {
		// No cache-dir mapping for this engine (e.g. a future ServiceMap entry
		// not yet added to EngineCacheDirs) -- no signal, fail open.
		return true
	}
	dir := filepath.Join(citadelCacheBaseDirFn(), cache.Dir)
	return cacheDirTotalSize(dir) > 0
}

// citadelCacheBaseDirFn resolves ~/citadel-cache, the fixed host path every
// embedded compose file's cache volume mount is rooted at
// (services.EngineCacheDirs, TestEngineCacheDirsMatchComposeMounts). A
// package var so tests point it at a temp directory instead of this
// machine's real home directory.
//
// Deliberately NOT internal/jobs.hfCacheBaseDir's env-override-aware
// resolution (HF_HUB_CACHE/HUGGINGFACE_HUB_CACHE/HF_HOME/XDG_CACHE_HOME):
// those affect where a HOST-SIDE `hf download` subprocess writes, but the
// compose bind-mount source the ENGINE CONTAINER actually reads from is this
// fixed path regardless of that subprocess's env -- and "does the container
// have weights to load" is exactly what this check answers. internal/status
// cannot import internal/jobs (jobs already imports status -- an import
// cycle), so this is a second, narrower resolver rather than a shared one.
var citadelCacheBaseDirFn = defaultCitadelCacheBaseDir

func defaultCitadelCacheBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "citadel-cache"
	}
	return filepath.Join(home, "citadel-cache")
}

// cacheDirTotalSize sums the sizes of every regular file under dir,
// recursively. Returns 0 if dir cannot be walked (in particular, if it does
// not exist -- the normal case for weights never pulled, or a directory
// Docker's own GC left untouched but an operator or disk-pressure GC (#682
// P5) swept). Deliberately re-implemented here rather than imported: the
// existing internal/jobs.dirTotalSize is unexported in a package
// internal/status cannot import (see citadelCacheBaseDirFn's comment).
func cacheDirTotalSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
