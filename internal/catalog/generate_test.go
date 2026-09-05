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

// The fabric MODULE_SET update-in-place path UNINSTALLS before it installs, and
// uninstall deletes <name>.env. Stickiness therefore cannot rely on install
// reading the file itself on that path -- the value must be carried across the
// teardown, or every re-assignment mints a new secret.
func TestCarryGeneratedConfigSurvivesTeardown(t *testing.T) {
	dir := t.TempDir()
	m := &ServiceManifest{
		Name: "nvr",
		Config: []ConfigVar{
			{Name: "NVR_MQTT_PASSWORD", Required: true, Generate: GenerateSecret},
			{Name: "NVR_CAMERAS", Required: true},
		},
	}
	path := filepath.Join(dir, "nvr.env")
	if err := writeEnvFile(path, map[string]string{
		"NVR_MQTT_PASSWORD": "already-minted",
		"NVR_CAMERAS":       "lab,garage",
	}); err != nil {
		t.Fatal(err)
	}

	// The assignment carries the operator's config but has no value for the
	// generated var -- that is the whole point of generating it.
	assigned := map[string]string{"NVR_CAMERAS": "lab,garage,driveway"}
	got := CarryGeneratedConfig(m, dir, assigned)

	if got["NVR_MQTT_PASSWORD"] != "already-minted" {
		t.Errorf("NVR_MQTT_PASSWORD = %q, want the persisted value carried across the teardown", got["NVR_MQTT_PASSWORD"])
	}
	// A non-generated var must NOT be resurrected from the old .env: the
	// assignment is authoritative for those, and reviving a stale camera list
	// would silently undo a config change.
	if got["NVR_CAMERAS"] != "lab,garage,driveway" {
		t.Errorf("NVR_CAMERAS = %q, want the assignment's value, not the persisted one", got["NVR_CAMERAS"])
	}
	// The caller's map must not be mutated -- it is also what gets recorded in
	// the lockfile for drift detection.
	if _, leaked := assigned["NVR_MQTT_PASSWORD"]; leaked {
		t.Error("CarryGeneratedConfig mutated the caller's config map; the lockfile would then record a secret and see drift forever")
	}
}

// An explicitly assigned value must beat the persisted one -- that is how a
// credential gets rotated on purpose.
func TestCarryGeneratedConfigYieldsToExplicitAssignment(t *testing.T) {
	dir := t.TempDir()
	m := &ServiceManifest{
		Name:   "nvr",
		Config: []ConfigVar{{Name: "S", Generate: GenerateSecret}},
	}
	if err := writeEnvFile(filepath.Join(dir, "nvr.env"), map[string]string{"S": "old"}); err != nil {
		t.Fatal(err)
	}

	got := CarryGeneratedConfig(m, dir, map[string]string{"S": "rotated"})
	if got["S"] != "rotated" {
		t.Errorf("S = %q, want the explicit assignment to win", got["S"])
	}
}

// A first install has no .env; carrying must be a clean no-op.
func TestCarryGeneratedConfigOnFirstInstall(t *testing.T) {
	m := &ServiceManifest{Name: "nvr", Config: []ConfigVar{{Name: "S", Generate: GenerateSecret}}}
	assigned := map[string]string{"NVR_CAMERAS": "lab"}

	got := CarryGeneratedConfig(m, t.TempDir(), assigned)
	if _, ok := got["S"]; ok {
		t.Error("carried a value for S with no .env present; install must mint it instead")
	}
	if got["NVR_CAMERAS"] != "lab" {
		t.Errorf("assignment config was dropped: %v", got)
	}
}

// TestModuleSetBridgeReassignCarriesAdminAndTenantKeys pins citadel#624
// sub-collision 2 through the update-in-place teardown path: a MODULE_SET
// reassignment of the bridge must preserve BOTH key classes across the
// uninstall-then-install that deletes <name>.env -- the `generate:` ADMIN_API_KEY
// AND the `carry:` TENANT_* keys (minted out of band by the bridge admin API, so
// they cannot be re-minted; dropping one rotates the platform-stored credential
// to nothing, the 401 class #624 exists to kill).
func TestModuleSetBridgeReassignCarriesAdminAndTenantKeys(t *testing.T) {
	dir := t.TempDir()
	m := &ServiceManifest{
		Name: "whatsapp-bridge",
		Config: []ConfigVar{
			{Name: "ADMIN_API_KEY", Required: true, Generate: GenerateSecret},
			{Name: "TENANT_API_KEY", Carry: true},
			{Name: "TENANT_ID", Carry: true},
			{Name: "TENANT_NAME", Carry: true},
			{Name: "BRIDGE_PORT"},
		},
	}
	if err := writeEnvFile(filepath.Join(dir, "whatsapp-bridge.env"), map[string]string{
		"ADMIN_API_KEY":  "wab_admin_persisted",
		"TENANT_API_KEY": "wab_tenant_persisted",
		"TENANT_ID":      "tenant-123",
		"TENANT_NAME":    "acme",
		"BRIDGE_PORT":    "8080",
	}); err != nil {
		t.Fatal(err)
	}

	// The reassignment carries no value for any of the sticky/carry keys.
	assigned := map[string]string{}
	got := CarryGeneratedConfig(m, dir, assigned)

	for k, want := range map[string]string{
		"ADMIN_API_KEY":  "wab_admin_persisted",
		"TENANT_API_KEY": "wab_tenant_persisted",
		"TENANT_ID":      "tenant-123",
		"TENANT_NAME":    "acme",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want the persisted value carried across the teardown (%q)", k, got[k], want)
		}
	}
	// A plain (non-generate, non-carry) var is NOT resurrected -- the assignment
	// stays authoritative for those.
	if _, revived := got["BRIDGE_PORT"]; revived {
		t.Errorf("BRIDGE_PORT was resurrected from the old .env (%q); only generate/carry vars carry", got["BRIDGE_PORT"])
	}
}

// TestCarryVarReusedOnNonTeardownInstallPath is advisor-point-2's regression: on
// the FIRST module install over a node whose bespoke stack already wrote the
// .env (the migration path), CarryGeneratedConfig never runs (the service is not
// yet in the manifest), so resolveConfig itself must reuse a persisted `carry:`
// value from `existing` -- exactly like a `generate:` var -- or the TENANT_* keys
// are dropped from the freshly written .env while ADMIN_API_KEY (generate-sticky)
// survives, silently reproducing the 401 drift on the transition.
func TestCarryVarReusedOnNonTeardownInstallPath(t *testing.T) {
	vars := []ConfigVar{
		{Name: "ADMIN_API_KEY", Required: true, Generate: GenerateSecret},
		{Name: "TENANT_API_KEY", Carry: true},
	}
	existing := map[string]string{
		"ADMIN_API_KEY":  "wab_admin_persisted",
		"TENANT_API_KEY": "wab_tenant_persisted",
	}
	got, err := resolveConfig(vars, nil, existing, false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if got["ADMIN_API_KEY"] != "wab_admin_persisted" {
		t.Errorf("ADMIN_API_KEY = %q, want the persisted (generate-sticky) value", got["ADMIN_API_KEY"])
	}
	if got["TENANT_API_KEY"] != "wab_tenant_persisted" {
		t.Errorf("TENANT_API_KEY = %q, want the persisted carry value reused directly by resolveConfig", got["TENANT_API_KEY"])
	}
}

// A carry var with nothing persisted (and no override) is LEFT UNSET -- unlike a
// generate var, the node cannot mint it. It must not become an empty-string
// credential, and an explicit override still wins.
func TestCarryVarNotMintedWhenAbsentButOverrideWins(t *testing.T) {
	vars := []ConfigVar{{Name: "TENANT_API_KEY", Carry: true}}

	got, err := resolveConfig(vars, nil, nil, false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if _, ok := got["TENANT_API_KEY"]; ok {
		t.Errorf("carry var was materialized to %q with nothing persisted; it must stay unset", got["TENANT_API_KEY"])
	}

	got2, err := resolveConfig(vars, map[string]string{"TENANT_API_KEY": "explicit"}, nil, false)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if got2["TENANT_API_KEY"] != "explicit" {
		t.Errorf("TENANT_API_KEY = %q, want the explicit override to win", got2["TENANT_API_KEY"])
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
