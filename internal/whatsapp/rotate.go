// internal/whatsapp/rotate.go
//
// Admin-key rotation for the WhatsApp bridge (citadel#624 part 3). The bridge's
// `/admin/*` control plane is guarded by ADMIN_API_KEY (X-Admin-Key); this file
// implements the one-and-only rotation primitive, shared verbatim by two entry
// points:
//
//   - the operator CLI `citadel whatsapp rotate-key` (cmd/whatsapp_rotate.go), and
//   - the WHATSAPP_PROVISION job's `rotate_admin_key: true` flag
//     (internal/worker/whatsapp_provision.go).
//
// Scope is the ADMIN key ONLY. The per-tenant data-plane keys (TENANT_API_KEY)
// are the credential the aceteam platform stores and hands to whatsapp_connect;
// rotating THEM would invalidate the platform-held api_key and needs a
// coordinated cross-repo change, so it is deliberately out of scope here.
//
// Persistence is unchanged: the same 0600 env file whatsapp.Provision already
// owns (SaveEnv). There is no separate "key store" -- inventing one would give
// the key two homes that could disagree.
package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// adminVerifyAttempts / adminVerifyBackoff bound the post-recreate verification
// retry. They are package vars (not consts) so tests can drop the backoff to
// zero rather than sleeping. A DEFINITIVE auth rejection (ErrAdminUnauthorized)
// never retries regardless -- only transient transport/timeout errors do.
var (
	adminVerifyAttempts = 4
	adminVerifyBackoff  = 2 * time.Second
)

// RotateResult is the structured outcome of an admin-key rotation. It carries
// ONLY the old/new fingerprints (sha256:<16 hex>), never the key bytes: the
// admin key never leaves the node, and a fingerprint is a one-way digest safe
// to log, return in a job result, and print to an operator.
type RotateResult struct {
	// Rotated is always true on a nil-error return (the key was generated,
	// persisted, the bridge recreated, and the new key verified). It exists so a
	// caller/consumer reads an explicit boolean rather than inferring success.
	Rotated bool
	// OldFingerprint is AdminKeyFingerprint of the key that was on disk before
	// rotation, or "" when none was set (a first key). Never the key bytes.
	OldFingerprint string
	// NewFingerprint is AdminKeyFingerprint of the freshly generated key. Never
	// the key bytes.
	NewFingerprint string
	// Port is the bridge's host port the verification talked to.
	Port int
}

// RotateBridgeClient is the subset of *Client the rotation VERIFY step needs. It
// deliberately does NOT reuse the provision flow's Health check: Health
// authenticates with the per-tenant X-API-Key, which would return 200 whether
// or not the ADMIN key rotated -- a structural false-green on the exact thing
// this gate exists to prove. ListTenants is the only admin-authed read on
// *Client (X-Admin-Key), so authenticating it with the NEW key is what proves
// the recreated container actually picked up the new admin secret.
type RotateBridgeClient interface {
	WaitReady(ctx context.Context, timeout time.Duration) error
	ListTenants(ctx context.Context) ([]map[string]any, error)
}

// Ensure the concrete client satisfies the interface.
var _ RotateBridgeClient = (*Client)(nil)

// RotateDeps injects the effectful edges (node dir resolution, bridge recreate,
// bridge client) so the core sequence stays unit-testable with in-memory stubs.
type RotateDeps struct {
	// ServicesDir returns the node's services directory (where the compose + env
	// file live). It MUST be a READ path (findAndReadManifest), never a
	// create-if-missing one: rotation on a node that has not deployed the bridge
	// is an error, not a bootstrap.
	ServicesDir func() (string, error)

	// RecreateBridge recreates the bridge container so it starts with the env
	// file's (now rotated) ADMIN_API_KEY. It must ride the #718 pull-before-up
	// behavior via startBridgeStack; RotateAdminKey persists the new env BEFORE
	// calling it, so the recreate observes the new key through --env-file. The
	// ctx is threaded through so a SIGTERM/root-cancel can reach the compose
	// subprocess (repo precedent #488) instead of being dropped.
	RecreateBridge func(ctx context.Context, servicesDir string) error

	// NewBridgeClient builds a RotateBridgeClient for the locally running bridge
	// at the given loopback port using the given admin key.
	NewBridgeClient func(port int, adminKey string) RotateBridgeClient

	// GenerateAdminKey mints the new admin secret. Defaults to GenerateAdminKey
	// (the SAME generator the provision path uses -- there is no second scheme).
	GenerateAdminKey func() (string, error)

	// ReadyTimeout bounds the post-recreate WaitReady poll. Zero uses 90s.
	ReadyTimeout time.Duration

	// Log reports progress. It MUST NOT be passed key bytes. Nil is a no-op.
	Log func(format string, args ...any)
}

// RotateAdminKey generates a new ADMIN_API_KEY, atomically rewrites the 0600 env
// file (preserving every other var), recreates the bridge so it picks up the new
// key, and verifies the new key authenticates against the bridge's admin control
// plane. On success it returns a RotateResult carrying the old and new
// fingerprints (never the key bytes).
//
// Failure ordering is deliberate and documented: the env is written FIRST, then
// the bridge is recreated, then verified. So any failure AFTER the atomic write
// (recreate error, WaitReady timeout, or the verify call failing) leaves the NEW
// key on disk -- the old key is gone. This is the accepted blast radius (admin
// control-plane access only; the per-tenant data plane in the bridge's own DB is
// untouched), and it is RECOVERABLE by simply re-running rotation (or `citadel
// whatsapp up`), which the verify-failure error says explicitly so the operator
// is never left guessing at a half-applied state.
//
// Competing-consumer race (documented, low severity, NOT coordinated here --
// mirrors how ReconcileOrphanedReservations documents its own worklock gap):
// `citadel whatsapp rotate-key` runs in a separate CLI process that holds NO
// worklock, while a concurrent WHATSAPP_PROVISION job (on `citadel work`'s
// serialized lane) does its own LoadEnv -> ... -> SaveEnv and would write back
// the OLD admin key it loaded before this rotation ran -- silently reverting a
// rotation this function already reported as succeeded. It is deliberately not
// closed with cross-process locking: the outcome is consistent (the bridge and
// the env file still agree, just on the old key) and re-running rotation
// recovers. The CLI additionally prints a best-effort warning when a `citadel
// work` worklock is currently held (cmd/whatsapp_rotate.go).
func RotateAdminKey(ctx context.Context, deps RotateDeps) (*RotateResult, error) {
	if deps.ServicesDir == nil || deps.RecreateBridge == nil || deps.NewBridgeClient == nil {
		return nil, fmt.Errorf("whatsapp.RotateAdminKey: ServicesDir, RecreateBridge and NewBridgeClient are required")
	}
	log := deps.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	genAdminKey := deps.GenerateAdminKey
	if genAdminKey == nil {
		genAdminKey = GenerateAdminKey
	}
	readyTimeout := deps.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = 90 * time.Second
	}

	servicesDir, err := deps.ServicesDir()
	if err != nil {
		return nil, fmt.Errorf("resolve node services dir: %w", err)
	}

	// Refuse when the bridge was never deployed: no env file means there is no
	// admin key to rotate, and minting one here would create a key with no
	// running bridge behind it -- a side effect the caller never asked for. This
	// check runs BEFORE any key generation, so a "no env file" node never has a
	// key generated as a side effect.
	envPath := EnvPath(servicesDir)
	if _, statErr := os.Stat(envPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, fmt.Errorf("no WhatsApp bridge config found at %s; run `citadel whatsapp up` to deploy the bridge before rotating its admin key", envPath)
		}
		return nil, fmt.Errorf("stat bridge config %s: %w", envPath, statErr)
	}

	env, err := LoadEnv(servicesDir)
	if err != nil {
		return nil, fmt.Errorf("read bridge config: %w", err)
	}

	oldFingerprint := AdminKeyFingerprint(env["ADMIN_API_KEY"])

	// Reuse the persisted host port; do NOT default to DefaultPort (8080), which
	// is citadel's own listener -- persistedBridgePort returns 0 for a missing or
	// invalid BRIDGE_PORT, and Provision writes BRIDGE_PORT on every deploy, so a
	// missing one is a malformed env, not a defaultable one. A wrong port here
	// would verify against the wrong service.
	port := persistedBridgePort(env)
	if port <= 0 {
		return nil, fmt.Errorf("bridge config at %s has no valid BRIDGE_PORT; cannot determine which port to verify the rotated key against (re-run `citadel whatsapp up` to repair the config)", envPath)
	}

	newKey, err := genAdminKey()
	if err != nil {
		return nil, fmt.Errorf("generate new admin key: %w", err)
	}
	newFingerprint := AdminKeyFingerprint(newKey)

	// Persist the new key atomically BEFORE recreate. Every OTHER var (TENANT_*,
	// BRIDGE_PORT, ...) is preserved because env is the full map loaded above and
	// only ADMIN_API_KEY is replaced. SaveEnv writes tempfile+rename at 0600, so a
	// crash mid-write never leaves a truncated env that would break the compose's
	// ${ADMIN_API_KEY:?} interpolation.
	env["ADMIN_API_KEY"] = newKey
	if err := SaveEnv(servicesDir, env); err != nil {
		if errors.Is(err, os.ErrPermission) {
			// The env file (and its dir) are typically owned by the root `citadel
			// work` worker; a non-root operator cannot write the tempfile or rename
			// it into place. Name the likely cause instead of a bare "permission
			// denied", and stop before any recreate.
			return nil, fmt.Errorf("persist rotated admin key: %w -- the bridge env file at %s is likely owned by the root `citadel work` worker; run `citadel whatsapp rotate-key` with the same privileges (e.g. sudo)", err, envPath)
		}
		return nil, fmt.Errorf("persist rotated admin key: %w", err)
	}
	log("admin key rotated on disk (fingerprint %s -> %s); recreating bridge to apply it", oldFingerprint, newFingerprint)

	// Recreate so the container restarts with the new key. Whether `up` actually
	// recreates the container depends on the bridge compose mapping ADMIN_API_KEY
	// into the container environment (that compose lives in the private
	// sunapi386/whatsapp-bridge repo, not readable here) -- the verify step below
	// is what catches it if a recreate silently no-ops, so this does not assert
	// the mechanism, only depends on it.
	if err := deps.RecreateBridge(ctx, servicesDir); err != nil {
		return nil, fmt.Errorf("recreate bridge after key rotation (the new admin key is already persisted; re-run rotation or `citadel whatsapp up` to retry): %w", err)
	}

	// Verify the NEW key authenticates against the admin control plane. WaitReady
	// first (the container just restarted), on its OWN readiness budget, then the
	// admin-authed ListTenants with the new key. The verify gets a FRESH
	// per-attempt timeout (httpTimeout) -- NOT the leftover slice of the readiness
	// budget WaitReady already consumed -- plus a bounded retry, so a slow cold
	// node cannot turn a 2s-leftover transport error into a false auth failure.
	client := deps.NewBridgeClient(port, newKey)
	readyCtx, cancelReady := context.WithTimeout(ctx, readyTimeout)
	readyErr := client.WaitReady(readyCtx, readyTimeout)
	cancelReady()
	if readyErr != nil {
		return nil, fmt.Errorf("bridge did not become ready after admin-key rotation (the new admin key is already on disk; re-run rotation or `citadel whatsapp up` once the bridge is up): %w", readyErr)
	}

	var lastErr error
	verified := false
	for attempt := 1; attempt <= adminVerifyAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, httpTimeout)
		_, err := client.ListTenants(attemptCtx)
		cancel()
		if err == nil {
			verified = true
			break
		}
		lastErr = err
		// A DEFINITIVE 401/403 will not self-heal -- report an auth failure and
		// stop (retrying a rejected key is pointless and would waste the budget).
		if errors.Is(err, ErrAdminUnauthorized) {
			return nil, fmt.Errorf("the rotated admin key was REJECTED by the bridge control plane (HTTP 401/403) -- the new key IS on disk and the bridge was recreated, but it did not take effect; re-run `citadel whatsapp rotate-key` (or `citadel whatsapp up`) to retry: %w", err)
		}
		// Transient (transport/timeout): back off and retry, unless the caller's
		// context is cancelled or we are out of attempts.
		if attempt == adminVerifyAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("admin-key rotation verification cancelled -- the new key IS on disk and the bridge was recreated; re-run `citadel whatsapp rotate-key` to re-verify: %w", ctx.Err())
		case <-time.After(adminVerifyBackoff):
		}
	}
	if !verified {
		// The check never COMPLETED (timeout/transport), so we must NOT assert an
		// auth failure -- say "could not verify" and that the key is written, so
		// the operator re-runs to re-verify rather than assuming the key is bad.
		return nil, fmt.Errorf("could not verify the rotated admin key against the bridge control plane -- the new key IS on disk and the bridge was recreated, but the check did not complete (timeout or transport error); re-run `citadel whatsapp rotate-key` to re-verify: %w", lastErr)
	}

	log("admin key rotation verified against the bridge control plane with the new key")
	return &RotateResult{
		Rotated:        true,
		OldFingerprint: oldFingerprint,
		NewFingerprint: newFingerprint,
		Port:           port,
	}, nil
}
