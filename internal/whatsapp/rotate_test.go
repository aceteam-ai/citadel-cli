package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeRotateBridge is an in-memory RotateBridgeClient for rotation tests. It
// records the admin key it was constructed with so a test can assert the VERIFY
// call authenticated with the NEW key.
type fakeRotateBridge struct {
	ready     error
	listErr   error
	adminKey  string
	listCalls int
}

func (f *fakeRotateBridge) WaitReady(ctx context.Context, timeout time.Duration) error {
	return f.ready
}
func (f *fakeRotateBridge) ListTenants(ctx context.Context) ([]map[string]any, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []map[string]any{}, nil
}

// flakyListBridge fails ListTenants transiently for the first failFirst calls,
// then succeeds -- to exercise the verify retry's recovery path.
type flakyListBridge struct {
	failFirst int
	calls     int
}

func (f *flakyListBridge) WaitReady(ctx context.Context, timeout time.Duration) error { return nil }
func (f *flakyListBridge) ListTenants(ctx context.Context) ([]map[string]any, error) {
	f.calls++
	if f.calls <= f.failFirst {
		return nil, errors.New("connection refused")
	}
	return []map[string]any{}, nil
}

// rotateTestDeps builds RotateDeps over a temp services dir with a deployed
// bridge env, a deterministic key generator, and a captured log. Individual
// tests override fields on the returned deps and inspect the captured state.
type rotateTestState struct {
	dir            string
	deps           RotateDeps
	recreateCalls  int
	verifyAdminKey string
	logs           []string
}

const rotateSentinelKey = "wab_admin_ROTATED_SENTINEL_KEY_bytes"

func rotateTestDeps(t *testing.T, env map[string]string) *rotateTestState {
	t.Helper()
	// Drop the verify backoff so transient-retry paths don't sleep in tests.
	prevBackoff := adminVerifyBackoff
	adminVerifyBackoff = 0
	t.Cleanup(func() { adminVerifyBackoff = prevBackoff })

	dir := t.TempDir()
	if env != nil {
		if err := SaveEnv(dir, env); err != nil {
			t.Fatalf("seed env: %v", err)
		}
	}
	st := &rotateTestState{dir: dir}
	st.deps = RotateDeps{
		ServicesDir:      func() (string, error) { return dir, nil },
		RecreateBridge:   func(ctx context.Context, servicesDir string) error { st.recreateCalls++; return nil },
		GenerateAdminKey: func() (string, error) { return rotateSentinelKey, nil },
		NewBridgeClient: func(port int, adminKey string) RotateBridgeClient {
			st.verifyAdminKey = adminKey
			return &fakeRotateBridge{}
		},
		Log: func(format string, args ...any) {
			st.logs = append(st.logs, fmt.Sprintf(format, args...))
		},
	}
	return st
}

func TestRotateAdminKey_HappyPath(t *testing.T) {
	st := rotateTestDeps(t, map[string]string{
		"ADMIN_API_KEY":  "wab_admin_old",
		"BRIDGE_PORT":    "8082",
		"TENANT_API_KEY": "wab_tenant_keep",
		"TENANT_ID":      "t_1",
		"TENANT_NAME":    "default",
	})

	res, err := RotateAdminKey(context.Background(), st.deps)
	if err != nil {
		t.Fatalf("RotateAdminKey() error = %v", err)
	}
	if !res.Rotated {
		t.Error("Rotated = false, want true")
	}
	if res.OldFingerprint != AdminKeyFingerprint("wab_admin_old") {
		t.Errorf("OldFingerprint = %q, want fingerprint of old key", res.OldFingerprint)
	}
	if res.NewFingerprint != AdminKeyFingerprint(rotateSentinelKey) {
		t.Errorf("NewFingerprint = %q, want fingerprint of new key", res.NewFingerprint)
	}
	if res.OldFingerprint == res.NewFingerprint {
		t.Error("fingerprint did not change across rotation")
	}
	if res.Port != 8082 {
		t.Errorf("Port = %d, want 8082 (persisted BRIDGE_PORT)", res.Port)
	}
	if st.recreateCalls != 1 {
		t.Errorf("RecreateBridge calls = %d, want 1", st.recreateCalls)
	}
	if st.verifyAdminKey != rotateSentinelKey {
		t.Errorf("verify authenticated with %q, want the NEW key", st.verifyAdminKey)
	}

	// The new key is persisted; every OTHER var is preserved byte-for-byte.
	got, err := LoadEnv(st.dir)
	if err != nil {
		t.Fatalf("LoadEnv after rotate: %v", err)
	}
	if got["ADMIN_API_KEY"] != rotateSentinelKey {
		t.Errorf("persisted ADMIN_API_KEY = %q, want the new key", got["ADMIN_API_KEY"])
	}
	for _, k := range []string{"BRIDGE_PORT", "TENANT_API_KEY", "TENANT_ID", "TENANT_NAME"} {
		if got[k] == "" {
			t.Errorf("preserved var %q was lost across rotation", k)
		}
	}
	if got["BRIDGE_PORT"] != "8082" || got["TENANT_API_KEY"] != "wab_tenant_keep" {
		t.Errorf("preserved vars changed: BRIDGE_PORT=%q TENANT_API_KEY=%q", got["BRIDGE_PORT"], got["TENANT_API_KEY"])
	}
}

// TestRotateAdminKey_Atomic0600NoTempLeak pins the atomic-write contract: the
// env file ends 0600 and no tempfile is left behind (a partial write would be a
// leftover temp or a truncated env).
func TestRotateAdminKey_Atomic0600NoTempLeak(t *testing.T) {
	st := rotateTestDeps(t, map[string]string{"ADMIN_API_KEY": "wab_admin_old", "BRIDGE_PORT": "8082"})

	if _, err := RotateAdminKey(context.Background(), st.deps); err != nil {
		t.Fatalf("RotateAdminKey() error = %v", err)
	}

	fi, err := os.Stat(EnvPath(st.dir))
	if err != nil {
		t.Fatalf("stat env: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("env file mode = %o, want 0600", perm)
	}

	entries, err := os.ReadDir(st.dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover tempfile after atomic write: %q", e.Name())
		}
	}
}

// TestRotateAdminKey_AuthRejectionIsDefinitive pins that a real 401/403
// (ErrAdminUnauthorized) is reported as a REJECTION, is NOT retried, and leaves
// the NEW key on disk (the deliberate failure-ordering contract).
func TestRotateAdminKey_AuthRejectionIsDefinitive(t *testing.T) {
	st := rotateTestDeps(t, map[string]string{"ADMIN_API_KEY": "wab_admin_old", "BRIDGE_PORT": "8082"})
	var fake *fakeRotateBridge
	st.deps.NewBridgeClient = func(port int, adminKey string) RotateBridgeClient {
		st.verifyAdminKey = adminKey
		fake = &fakeRotateBridge{listErr: fmt.Errorf("list tenants failed (HTTP 401): nope: %w", ErrAdminUnauthorized)}
		return fake
	}

	_, err := RotateAdminKey(context.Background(), st.deps)
	if err == nil {
		t.Fatal("expected error on auth rejection, got nil")
	}
	if !strings.Contains(err.Error(), "REJECTED") {
		t.Errorf("auth-rejection error should say REJECTED, got: %v", err)
	}
	if !strings.Contains(err.Error(), "on disk") {
		t.Errorf("error should tell the operator the new key is on disk, got: %v", err)
	}
	if fake.listCalls != 1 {
		t.Errorf("a definitive 401 must NOT be retried; ListTenants calls = %d, want 1", fake.listCalls)
	}
	got, _ := LoadEnv(st.dir)
	if got["ADMIN_API_KEY"] != rotateSentinelKey {
		t.Errorf("after auth rejection ADMIN_API_KEY = %q, want the NEW key (no rollback)", got["ADMIN_API_KEY"])
	}
}

// TestRotateAdminKey_TransientVerifyIsInconclusive pins that a transport/timeout
// error (NOT ErrAdminUnauthorized) is retried up to the attempt bound and then
// reported as "could not verify" -- NEVER asserted as an auth failure -- with the
// new key still on disk.
func TestRotateAdminKey_TransientVerifyIsInconclusive(t *testing.T) {
	st := rotateTestDeps(t, map[string]string{"ADMIN_API_KEY": "wab_admin_old", "BRIDGE_PORT": "8082"})
	var fake *fakeRotateBridge
	st.deps.NewBridgeClient = func(port int, adminKey string) RotateBridgeClient {
		fake = &fakeRotateBridge{listErr: errors.New("dial tcp 127.0.0.1:8082: connect: connection refused")}
		return fake
	}

	_, err := RotateAdminKey(context.Background(), st.deps)
	if err == nil {
		t.Fatal("expected error on transient verify failure, got nil")
	}
	if !strings.Contains(err.Error(), "could not verify") {
		t.Errorf("transient error must read as 'could not verify', got: %v", err)
	}
	if strings.Contains(err.Error(), "REJECTED") {
		t.Errorf("a transient error must NOT be reported as an auth rejection, got: %v", err)
	}
	if fake.listCalls != adminVerifyAttempts {
		t.Errorf("a transient error should retry to the bound; ListTenants calls = %d, want %d", fake.listCalls, adminVerifyAttempts)
	}
	got, _ := LoadEnv(st.dir)
	if got["ADMIN_API_KEY"] != rotateSentinelKey {
		t.Errorf("after transient failure ADMIN_API_KEY = %q, want the NEW key (no rollback)", got["ADMIN_API_KEY"])
	}
}

// TestRotateAdminKey_VerifyRecoversAfterTransientRetry pins that a verify that
// fails transiently then succeeds within the bound reports success.
func TestRotateAdminKey_VerifyRecoversAfterTransientRetry(t *testing.T) {
	st := rotateTestDeps(t, map[string]string{"ADMIN_API_KEY": "wab_admin_old", "BRIDGE_PORT": "8082"})
	flaky := &flakyListBridge{failFirst: 2}
	st.deps.NewBridgeClient = func(port int, adminKey string) RotateBridgeClient { return flaky }

	res, err := RotateAdminKey(context.Background(), st.deps)
	if err != nil {
		t.Fatalf("expected success after transient retries, got %v", err)
	}
	if !res.Rotated {
		t.Error("Rotated = false, want true")
	}
	if flaky.calls <= 2 {
		t.Errorf("expected a retry past the transient failures; calls = %d", flaky.calls)
	}
}

// TestRotateAdminKey_NoEnvFileFailsCleanly pins that rotating on a node with no
// bridge env file fails cleanly and mints NO key as a side effect.
func TestRotateAdminKey_NoEnvFileFailsCleanly(t *testing.T) {
	st := rotateTestDeps(t, nil) // no SaveEnv -> no env file

	res, err := RotateAdminKey(context.Background(), st.deps)
	if err == nil {
		t.Fatal("expected error when no env file present, got nil")
	}
	if res != nil {
		t.Errorf("result should be nil on error, got %+v", res)
	}
	if st.recreateCalls != 0 {
		t.Errorf("RecreateBridge must not be called when there is no env file (got %d calls)", st.recreateCalls)
	}
	if _, statErr := os.Stat(EnvPath(st.dir)); !os.IsNotExist(statErr) {
		t.Error("an env file was created as a side effect of a failed rotation; must not mint a key")
	}
}

// TestRotateAdminKey_MissingBridgePortFails pins that a malformed env with no
// BRIDGE_PORT is refused rather than defaulting to 8080 (citadel's own port).
func TestRotateAdminKey_MissingBridgePortFails(t *testing.T) {
	st := rotateTestDeps(t, map[string]string{"ADMIN_API_KEY": "wab_admin_old"}) // no BRIDGE_PORT

	_, err := RotateAdminKey(context.Background(), st.deps)
	if err == nil {
		t.Fatal("expected error when BRIDGE_PORT missing, got nil")
	}
	if !strings.Contains(err.Error(), "BRIDGE_PORT") {
		t.Errorf("error should name BRIDGE_PORT, got: %v", err)
	}
	if st.recreateCalls != 0 {
		t.Errorf("RecreateBridge must not run when the port is unresolved (got %d)", st.recreateCalls)
	}
}

// TestRotateAdminKey_EmptyOldKeyEmptyFingerprint pins the "" in / "" out
// fingerprint contract: an env file present but with no ADMIN_API_KEY yields an
// empty OLD fingerprint (never hash("")), and rotation still succeeds.
func TestRotateAdminKey_EmptyOldKeyEmptyFingerprint(t *testing.T) {
	st := rotateTestDeps(t, map[string]string{"BRIDGE_PORT": "8082"}) // env exists, no admin key

	res, err := RotateAdminKey(context.Background(), st.deps)
	if err != nil {
		t.Fatalf("RotateAdminKey() error = %v", err)
	}
	if res.OldFingerprint != "" {
		t.Errorf("OldFingerprint = %q, want \"\" for an absent prior key", res.OldFingerprint)
	}
	if res.NewFingerprint == "" {
		t.Error("NewFingerprint is empty; want the fingerprint of the freshly minted key")
	}
}

// TestRotateAdminKey_NoSecretBytesLeak scans the result fingerprints, the error,
// and every captured log line across the success, verify-failure, AND
// recreate-failure branches for the raw key bytes. The recreate-failure branch
// matters most: it is the one path whose error wraps a third-party subprocess's
// output (docker's stdout via startBridgeStack), so it is the likeliest place a
// secret could ride out. The fingerprint (a one-way digest) may appear; the key
// bytes must never appear in the result, the error, or any log line. (The on-disk
// env file legitimately holds the key and is NOT scanned.)
func TestRotateAdminKey_NoSecretBytesLeak(t *testing.T) {
	scan := func(t *testing.T, where string, s string) {
		if strings.Contains(s, rotateSentinelKey) {
			t.Errorf("admin key bytes leaked into %s: %q", where, s)
		}
	}

	t.Run("success", func(t *testing.T) {
		st := rotateTestDeps(t, map[string]string{"ADMIN_API_KEY": "wab_admin_old", "BRIDGE_PORT": "8082"})
		res, err := RotateAdminKey(context.Background(), st.deps)
		if err != nil {
			t.Fatalf("RotateAdminKey() error = %v", err)
		}
		scan(t, "result.OldFingerprint", res.OldFingerprint)
		scan(t, "result.NewFingerprint", res.NewFingerprint)
		for i, line := range st.logs {
			scan(t, fmt.Sprintf("log[%d]", i), line)
		}
	})

	t.Run("verify_failure", func(t *testing.T) {
		st := rotateTestDeps(t, map[string]string{"ADMIN_API_KEY": "wab_admin_old", "BRIDGE_PORT": "8082"})
		st.deps.NewBridgeClient = func(port int, adminKey string) RotateBridgeClient {
			return &fakeRotateBridge{listErr: fmt.Errorf("rejected: %w", ErrAdminUnauthorized)}
		}
		_, err := RotateAdminKey(context.Background(), st.deps)
		if err == nil {
			t.Fatal("expected verify failure")
		}
		scan(t, "error", err.Error())
		for i, line := range st.logs {
			scan(t, fmt.Sprintf("log[%d]", i), line)
		}
	})

	t.Run("recreate_failure", func(t *testing.T) {
		st := rotateTestDeps(t, map[string]string{"ADMIN_API_KEY": "wab_admin_old", "BRIDGE_PORT": "8082"})
		// A realistic docker recreate error (compose echoes the variable NAME on a
		// ${VAR:?} failure, never the value). The rotation must not pull the key
		// value in FROM the env it just wrote and splice it into the error/logs.
		st.deps.RecreateBridge = func(ctx context.Context, servicesDir string) error {
			return errors.New("docker compose up failed: required variable ADMIN_API_KEY is missing")
		}
		_, err := RotateAdminKey(context.Background(), st.deps)
		if err == nil {
			t.Fatal("expected recreate failure")
		}
		scan(t, "error", err.Error())
		for i, line := range st.logs {
			scan(t, fmt.Sprintf("log[%d]", i), line)
		}
	})
}

// TestSaveEnvAtomicPreservesContent is a direct check that the atomic writer
// produces a complete, parseable file (a partial write would fail to round-trip).
func TestSaveEnvAtomicPreservesContent(t *testing.T) {
	dir := t.TempDir()
	in := map[string]string{"ADMIN_API_KEY": "a", "BRIDGE_PORT": "8082", "TENANT_API_KEY": "wab_x"}
	if err := SaveEnv(dir, in); err != nil {
		t.Fatalf("SaveEnv: %v", err)
	}
	got, err := LoadEnv(dir)
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	for k, v := range in {
		if got[k] != v {
			t.Errorf("round-trip %q = %q, want %q", k, got[k], v)
		}
	}
	// No tempfile left behind next to the env file.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover tempfile: %q", e.Name())
		}
	}
}
