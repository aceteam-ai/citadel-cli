package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A `generate:` var must satisfy `required:` without an override, an operator
// prompt, or a shared default. This is the whole point: a fabric MODULE_SET
// assignment and `citadel module update` both resolve config NON-INTERACTIVELY
// with no value for such a var, and before generation existed that was a hard
// "required config X has no value" failure -- i.e. the module became
// undeployable the moment it declared an internal credential.
func TestGeneratedSecretSatisfiesRequiredNonInteractively(t *testing.T) {
	vars := []ConfigVar{{Name: "BROKER_PASSWORD", Required: true, Generate: GenerateSecret}}

	got, err := resolveConfig(vars, nil, nil, false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if got["BROKER_PASSWORD"] == "" {
		t.Fatal("BROKER_PASSWORD is empty; a generated var must never yield a blank credential")
	}
	// Shell/compose safety: the value is interpolated into compose env and a
	// quoted entrypoint, so it must carry no separator or metacharacter.
	if strings.ContainsAny(got["BROKER_PASSWORD"], "=\"'$` \t\n") {
		t.Errorf("generated secret %q contains a character unsafe for compose/shell interpolation", got["BROKER_PASSWORD"])
	}
	if len(got["BROKER_PASSWORD"]) < 32 {
		t.Errorf("generated secret is only %d chars; want a high-entropy value", len(got["BROKER_PASSWORD"]))
	}
}

// Two independent nodes must not end up with the same credential -- the failure
// mode a static `default:` would have.
func TestGeneratedSecretDiffersPerInstall(t *testing.T) {
	vars := []ConfigVar{{Name: "S", Generate: GenerateSecret}}

	a, err := resolveConfig(vars, nil, nil, false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	b, err := resolveConfig(vars, nil, nil, false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if a["S"] == b["S"] {
		t.Error("two installs produced the same secret; every node must mint its own")
	}
}

// Stickiness. A reconcile or `citadel module update` re-runs install; if that
// re-minted the secret, the .env would disagree with the credential the running
// broker container was started with, and Frigate would silently fail to connect
// until the whole stack was recreated.
func TestGeneratedSecretIsReusedFromExistingEnv(t *testing.T) {
	vars := []ConfigVar{{Name: "S", Required: true, Generate: GenerateSecret}}

	first, err := resolveConfig(vars, nil, nil, false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	second, err := resolveConfig(vars, nil, first, false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if second["S"] != first["S"] {
		t.Errorf("secret rotated across installs: %q -> %q", first["S"], second["S"])
	}
}

// An explicit value still wins over both generation and any persisted value --
// that is how an operator points the module at an external broker.
func TestOverrideBeatsGenerateAndExisting(t *testing.T) {
	vars := []ConfigVar{{Name: "S", Generate: GenerateSecret}}

	got, err := resolveConfig(vars,
		map[string]string{"S": "operator-supplied"},
		map[string]string{"S": "previously-generated"},
		false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if got["S"] != "operator-supplied" {
		t.Errorf("S = %q, want the operator override to win", got["S"])
	}
}

// A typo'd kind must fail loudly. Silently returning "" would hand the module an
// empty password and, for a broker, that is an unauthenticated listener.
func TestUnknownGenerateKindIsAnError(t *testing.T) {
	vars := []ConfigVar{{Name: "S", Generate: "sekret"}}

	if _, err := resolveConfig(vars, nil, nil, false); err == nil {
		t.Fatal("resolveConfig accepted an unknown generate kind; want an error")
	}
}

// Round-trip through the real .env file, since stickiness depends on install
// being able to READ BACK what writeEnvFile produced.
func TestEnvFileRoundTripPreservesGeneratedSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nvr.env")

	vars := []ConfigVar{{Name: "S", Required: true, Generate: GenerateSecret}}
	first, err := resolveConfig(vars, nil, readEnvFile(path), false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if err := writeEnvFile(path, first); err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}

	second, err := resolveConfig(vars, nil, readEnvFile(path), false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if second["S"] != first["S"] {
		t.Errorf("secret did not survive the .env round trip: %q -> %q", first["S"], second["S"])
	}
}

// readEnvFile must not choke on the shapes a real .env grows: comments, blanks,
// and values that legitimately contain '=' (base64 padding, query strings).
func TestReadEnvFileTolerance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.env")
	if err := os.WriteFile(path, []byte("# a comment\n\nA=1\nB=v=with=equals\nnot-a-pair\n"), 0600); err != nil {
		t.Fatal(err)
	}

	got := readEnvFile(path)
	if got["A"] != "1" {
		t.Errorf("A = %q, want 1", got["A"])
	}
	if got["B"] != "v=with=equals" {
		t.Errorf("B = %q, want the full value after the FIRST = only", got["B"])
	}
	if len(got) != 2 {
		t.Errorf("parsed %d entries (%v), want 2", len(got), got)
	}

	// A missing file is "nothing persisted yet", not a failure.
	if len(readEnvFile(filepath.Join(dir, "absent.env"))) != 0 {
		t.Error("a missing .env must parse as empty, not error")
	}
}
