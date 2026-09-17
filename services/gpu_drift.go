package services

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// gpu_drift.go heals the citadel-cli#1069 gap: a macOS node that materialized an
// embedded engine compose (ollama/llamacpp) under a pre-#1064 binary keeps the
// STALE, nvidia-reserving file on disk after upgrading. #1064 fixed the darwin
// templates (the darwin build's ServiceMap[ollama]/[llamacpp] now embed the
// CPU/arm64 variants with the GPU reservation dropped), but the two ensure*
// materialization fast paths intentionally leave an already-materialized compose
// file untouched (the #860 create-once rule). So `docker compose up` for those
// engines keeps failing with `could not select device driver "nvidia"` until the
// file is deleted and re-materialized.
//
// composerefresh.Sweep already re-materializes these files on a version-changed
// `citadel work` boot (on darwin ServiceMap holds the variant, and #1064
// registered the variant hash in KnownComposeHashes -- see
// TestKnownComposeHashesCoverDarwinVariants). But Sweep is version-gated and only
// runs at boot; it never covers `citadel run <service>` or a worker SERVICE_START
// that materializes through the ensure* fast paths on an already-current stamp.
// This is the start-path complement, mirroring the #1030 loopback-bind drift
// heal (shouldRecreateForEngineBindDrift, cmd/compose_refresh.go) -- itself a
// start-path heal, not part of Sweep.
//
// The heal is deliberately narrow and cannot clobber operator hand-edits: it
// rewrites a file only when its content is BYTE-IDENTICAL to a template citadel
// is known to have shipped (KnownComposeHashes), exactly the #426/#1030
// preservation discipline. It is a no-op by construction on any non-darwin build:
// the `current` template it is handed there still carries the nvidia reservation,
// so ShouldHealStaleGPUReservation short-circuits (linux/windows byte-identical).

// composeDeclaresNvidiaReservation reports whether compose YAML content declares
// an nvidia GPU device reservation under any service's
// deploy.resources.reservations.devices[].driver. Both the on-disk and the
// current-template inputs are citadel-authored templates (the heal only ever
// runs against hash-matched known content), so a YAML parse is reliable; a parse
// failure conservatively reports false (no reservation detected => no heal).
func composeDeclaresNvidiaReservation(content string) bool {
	var doc struct {
		Services map[string]struct {
			Deploy struct {
				Resources struct {
					Reservations struct {
						Devices []struct {
							Driver string `yaml:"driver"`
						} `yaml:"devices"`
					} `yaml:"reservations"`
				} `yaml:"resources"`
			} `yaml:"deploy"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return false
	}
	for _, svc := range doc.Services {
		for _, d := range svc.Deploy.Resources.Reservations.Devices {
			if strings.EqualFold(strings.TrimSpace(d.Driver), "nvidia") {
				return true
			}
		}
	}
	return false
}

// composeContentHash is the sha256 hex of compose content, matching the encoding
// used by KnownComposeHashes and internal/composerefresh.
func composeContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// ShouldHealStaleGPUReservation decides whether onDisk (an already-materialized
// embedded-engine compose) should be re-materialized to current because it is a
// stale, citadel-written template that still reserves an nvidia device that the
// current template has dropped (the citadel-cli#1069 darwin drift).
//
// current is services.ServiceMap[service] resolved for THIS build -- the darwin
// CPU/arm64 variant on a darwin build, the nvidia-reserving linux template
// elsewhere. knownHashes is services.KnownComposeHashes[service].
//
// Returns true only when ALL hold:
//   - current does NOT declare an nvidia reservation. On a non-darwin build the
//     engine template still reserves nvidia, so this is always false there -- the
//     linux/windows path is byte-identical by construction.
//   - onDisk DOES declare an nvidia reservation (the drift this heals).
//   - onDisk differs from current (i.e. not already healed).
//   - onDisk is byte-identical to a hash in knownHashes: provably a template
//     citadel shipped, never an operator hand-edit. A hand-edited file (nvidia
//     block plus any operator change) hashes to nothing in the set and is
//     preserved untouched -- the #426/#1030 discipline.
//
// Pure and OS-agnostic (current is injected), so it is fully unit-testable on the
// linux CI host.
func ShouldHealStaleGPUReservation(onDisk, current string, knownHashes map[string]bool) bool {
	if composeDeclaresNvidiaReservation(current) {
		return false
	}
	if !composeDeclaresNvidiaReservation(onDisk) {
		return false
	}
	onDiskHash := composeContentHash(onDisk)
	if onDiskHash == composeContentHash(current) {
		return false
	}
	return knownHashes[onDiskHash]
}

// HealStaleGPUReservationOnDisk reads destPath and, when
// ShouldHealStaleGPUReservation is satisfied, rewrites it to current (0600, the
// same perms the ensure* materialize paths use). Returns whether it rewrote the
// file. A missing file, an unchanged (already-current or hand-edited) file, and
// any non-darwin build (where current still reserves nvidia) are all no-op
// (false, nil). Callers should still gate the invocation on the build being
// darwin and the service being an embedded engine so the common healthy path
// pays nothing; this function is safe to call unconditionally regardless.
func HealStaleGPUReservationOnDisk(destPath, current string, knownHashes map[string]bool) (bool, error) {
	onDisk, err := os.ReadFile(destPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !ShouldHealStaleGPUReservation(string(onDisk), current, knownHashes) {
		return false, nil
	}
	if err := os.WriteFile(destPath, []byte(current), 0o600); err != nil {
		return false, err
	}
	return true, nil
}
