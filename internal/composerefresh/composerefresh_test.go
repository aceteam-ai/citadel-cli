package composerefresh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// embeddedV2 is what the current (upgraded) binary would materialize. embeddedV1
// is the stale template a previous binary wrote. They differ so a refresh is
// observable.
const (
	embeddedV1 = "services:\n  llamacpp:\n    ports:\n      - \"8080:8080\"\n"
	embeddedV2 = "services:\n  llamacpp:\n    ports:\n      - \"${CITADEL_LLAMACPP_HOST_PORT:?}:8080\"\n"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readStampFile(t *testing.T, dir string) managedStamp {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, StampFileName))
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	var s managedStamp
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal stamp: %v", err)
	}
	return s
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// baseOpts builds an Options for a single citadel-owned service (llamacpp) whose
// current template is embeddedV2 and whose host port is managed. No recreator
// (file-refresh only) unless a test overrides it.
func baseOpts(dir, version string) Options {
	return Options{
		ServicesDir: dir,
		Version:     version,
		Embedded:    map[string]string{"llamacpp": embeddedV2},
		PortManaged: map[string]int{"llamacpp": 8200},
	}
}

// (a) An upgrade rewrites a citadel-managed file that matches the recorded hash.
func TestSweep_RefreshesCitadelManagedFile(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "llamacpp.yml")

	// A previous binary wrote embeddedV1 and stamped its hash at version v1.
	writeFile(t, composePath, embeddedV1)
	prevStamp := managedStamp{Version: "v2.50.0", Hashes: map[string]string{"llamacpp": sha(embeddedV1)}}
	if err := writeStamp(dir, prevStamp); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}

	res, err := Sweep(baseOpts(dir, "v2.57.0"))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Skipped {
		t.Fatal("expected sweep to run (version changed), got Skipped")
	}
	if !contains(res.Refreshed, "llamacpp") {
		t.Fatalf("expected llamacpp refreshed, got %+v", res)
	}
	got, _ := os.ReadFile(composePath)
	if string(got) != embeddedV2 {
		t.Fatalf("file not refreshed to new template:\n%s", got)
	}
	// Stamp is updated to the new version + new hash.
	stamp := readStampFile(t, dir)
	if stamp.Version != "v2.57.0" {
		t.Fatalf("stamp version = %q, want v2.57.0", stamp.Version)
	}
	if stamp.Hashes["llamacpp"] != sha(embeddedV2) {
		t.Fatal("stamp hash not updated to new template hash")
	}
}

// (b) An upgrade PRESERVES a hand-edited (hash-mismatched) file.
func TestSweep_PreservesHandEditedFile(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "llamacpp.yml")

	handEdited := embeddedV1 + "\n# operator tweak: pinned image\n"
	writeFile(t, composePath, handEdited)
	// The stamp records the ORIGINAL embeddedV1 hash; on-disk differs => edited.
	prevStamp := managedStamp{Version: "v2.50.0", Hashes: map[string]string{"llamacpp": sha(embeddedV1)}}
	if err := writeStamp(dir, prevStamp); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}

	res, err := Sweep(baseOpts(dir, "v2.57.0"))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if contains(res.Refreshed, "llamacpp") {
		t.Fatal("hand-edited file was refreshed; must be preserved")
	}
	if !contains(res.Preserved, "llamacpp") {
		t.Fatalf("expected llamacpp preserved, got %+v", res)
	}
	got, _ := os.ReadFile(composePath)
	if string(got) != handEdited {
		t.Fatal("hand-edited file content changed")
	}
	// The recorded hash tracks the CURRENT on-disk (edited) content, so a future
	// clean upgrade can re-adopt the file if the operator restores a template.
	stamp := readStampFile(t, dir)
	if stamp.Hashes["llamacpp"] != sha(handEdited) {
		t.Fatal("recorded hash for hand-edited file should track on-disk content")
	}
}

// (b') First-upgrade bootstrap: pre-#426 nodes have NO stamp. Their stale
// citadel-owned composes must still be refreshed (this is the whole target
// population of the issue).
func TestSweep_BootstrapNoStampRefreshes(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "llamacpp.yml")
	writeFile(t, composePath, embeddedV1) // stale, no stamp exists

	res, err := Sweep(baseOpts(dir, "v2.57.0"))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Skipped {
		t.Fatal("expected sweep to run on a no-stamp node")
	}
	if !contains(res.Refreshed, "llamacpp") {
		t.Fatalf("no-stamp stale file must be refreshed, got %+v", res)
	}
	got, _ := os.ReadFile(composePath)
	if string(got) != embeddedV2 {
		t.Fatal("no-stamp stale file was not refreshed")
	}
	// A stamp is now written so subsequent upgrades use precise hash matching.
	if _, existed := readStamp(dir); !existed {
		t.Fatal("expected a stamp to be written after bootstrap refresh")
	}
}

// (b”) Bootstrap with a known-historical-hash allowlist: a no-stamp file that
// matches a prior shipped template is refreshed; one that matches nothing is
// preserved as an operator edit.
func TestSweep_BootstrapKnownHashAllowlist(t *testing.T) {
	dir := t.TempDir()

	// llamacpp: stale but byte-identical to a known prior template -> refresh.
	managedPath := filepath.Join(dir, "llamacpp.yml")
	writeFile(t, managedPath, embeddedV1)
	// vllm: hand-edited, matches no known template -> preserve.
	handEdited := "services:\n  vllm:\n    image: acme/pinned\n"
	vllmPath := filepath.Join(dir, "vllm.yml")
	writeFile(t, vllmPath, handEdited)

	res, err := Sweep(Options{
		ServicesDir: dir,
		Version:     "v2.57.0",
		Embedded: map[string]string{
			"llamacpp": embeddedV2,
			"vllm":     "services:\n  vllm:\n    image: new\n",
		},
		PortManaged: map[string]int{"llamacpp": 8200, "vllm": 8201},
		KnownHistoricalHashes: map[string]map[string]bool{
			"llamacpp": {sha(embeddedV1): true},
			// vllm: only the current template is "known"; handEdited is not.
		},
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !contains(res.Refreshed, "llamacpp") {
		t.Fatalf("known-hash stale file must be refreshed, got %+v", res)
	}
	if !contains(res.Preserved, "vllm") {
		t.Fatalf("unknown-hash edited file must be preserved, got %+v", res)
	}
	if got, _ := os.ReadFile(vllmPath); string(got) != handEdited {
		t.Fatal("hand-edited vllm was clobbered under known-hash bootstrap")
	}
}

// (c) Non-citadel / operator files are never touched.
func TestSweep_NeverTouchesOperatorFiles(t *testing.T) {
	dir := t.TempDir()
	// A file NOT in Embedded (operator-authored service compose).
	operatorPath := filepath.Join(dir, "my-custom-thing.yml")
	operatorContent := "services:\n  custom:\n    image: acme/custom\n"
	writeFile(t, operatorPath, operatorContent)
	// A sandbox override sibling of a managed service must also be untouched.
	sandboxPath := filepath.Join(dir, "llamacpp.sandbox.yml")
	sandboxContent := "services:\n  llamacpp:\n    read_only: true\n"
	writeFile(t, sandboxPath, sandboxContent)
	// The managed service itself is absent -> gets materialized.
	res, err := Sweep(baseOpts(dir, "v2.57.0"))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got, _ := os.ReadFile(operatorPath); string(got) != operatorContent {
		t.Fatal("operator-authored compose was modified")
	}
	if got, _ := os.ReadFile(sandboxPath); string(got) != sandboxContent {
		t.Fatal("sandbox override was modified")
	}
	// And llamacpp was materialized (absent-file path).
	if !contains(res.Refreshed, "llamacpp") {
		t.Fatalf("absent managed file should be materialized, got %+v", res)
	}
}

// (d) An unchanged-version boot is a no-op (no file writes, Skipped=true).
func TestSweep_UnchangedVersionIsNoOp(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "llamacpp.yml")
	writeFile(t, composePath, embeddedV1) // deliberately stale content

	stamp := managedStamp{Version: "v2.57.0", Hashes: map[string]string{"llamacpp": sha(embeddedV2)}}
	if err := writeStamp(dir, stamp); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}
	before, _ := os.Stat(composePath)

	res, err := Sweep(baseOpts(dir, "v2.57.0"))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !res.Skipped {
		t.Fatal("expected Skipped on unchanged version")
	}
	// File must be untouched even though its content is stale (version gate wins).
	if got, _ := os.ReadFile(composePath); string(got) != embeddedV1 {
		t.Fatal("file was modified on an unchanged-version boot")
	}
	after, _ := os.Stat(composePath)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("file mtime changed on a no-op boot")
	}
}

// (e) The version stamp is read and written correctly across a round trip.
func TestSweep_StampRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// No stamp initially.
	if _, existed := readStamp(dir); existed {
		t.Fatal("expected no stamp initially")
	}

	if _, err := Sweep(baseOpts(dir, "v2.57.0")); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	s, existed := readStamp(dir)
	if !existed {
		t.Fatal("stamp not written")
	}
	if s.Version != "v2.57.0" {
		t.Fatalf("stamp version = %q, want v2.57.0", s.Version)
	}
	if s.Hashes["llamacpp"] != sha(embeddedV2) {
		t.Fatal("stamp hash mismatch after write")
	}

	// A second sweep at the SAME version is skipped.
	res2, err := Sweep(baseOpts(dir, "v2.57.0"))
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if !res2.Skipped {
		t.Fatal("second sweep at same version should be skipped")
	}
}

// Recreate is only attempted on a port-managed service whose file was refreshed,
// and only when a Recreator is wired in. Uses a fake recreator (no docker).
func TestSweep_RecreateOnlyOnPortManagedRefresh(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "llamacpp.yml"), embeddedV1)
	writeFile(t, filepath.Join(dir, "ollama.yml"), "services:\n  ollama:\n    image: old\n")

	var calls []string
	opts := Options{
		ServicesDir: dir,
		Version:     "v2.57.0",
		Embedded: map[string]string{
			"llamacpp": embeddedV2,
			// ollama refreshes too, but it is NOT port-managed.
			"ollama": "services:\n  ollama:\n    image: new\n",
		},
		PortManaged: map[string]int{"llamacpp": 8200},
		Recreator: func(service, composePath string, wantHostPort int) (bool, error) {
			calls = append(calls, service)
			return true, nil // pretend the port moved and we recreated
		},
	}

	res, err := Sweep(opts)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(calls) != 1 || calls[0] != "llamacpp" {
		t.Fatalf("recreator should be called only for port-managed llamacpp, got %v", calls)
	}
	if !contains(res.Recreated, "llamacpp") {
		t.Fatalf("expected llamacpp recreated, got %+v", res)
	}
	if contains(res.Recreated, "ollama") {
		t.Fatal("non-port-managed ollama must never be recreated")
	}
}

// A failed force-recreate rolls the compose file back to its prior content so a
// bad template can't brick the service AND destroy the working file.
func TestSweep_RecreateFailureRollsBackFile(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "llamacpp.yml")
	writeFile(t, composePath, embeddedV1)

	opts := baseOpts(dir, "v2.57.0")
	opts.Recreator = func(service, composePath string, wantHostPort int) (bool, error) {
		return false, errThrow("compose up failed: boom")
	}

	res, err := Sweep(opts)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if contains(res.Refreshed, "llamacpp") {
		t.Fatal("a rolled-back refresh must not be reported as Refreshed")
	}
	// File restored to the prior content.
	if got, _ := os.ReadFile(composePath); string(got) != embeddedV1 {
		t.Fatalf("file not rolled back after recreate failure:\n%s", got)
	}
	// Stamp records the restored (prior) content's hash so the next boot retries.
	stamp := readStampFile(t, dir)
	if stamp.Hashes["llamacpp"] != sha(embeddedV1) {
		t.Fatal("stamp hash should track the rolled-back content")
	}
}

type errThrow string

func (e errThrow) Error() string { return string(e) }
