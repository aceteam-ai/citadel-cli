package reconcile

import "context"

// ModuleOps is the narrow side-effect surface the reconcile engine drives. It
// wraps the EXISTING module-install / run / stop / uninstall operations so the
// engine can converge a node without importing docker, git, or the catalog
// install internals directly — which keeps the diff/converge logic fully
// unit-testable with a fake.
//
// The live adapter (a LATER increment) maps these onto:
//   - Install  -> catalog.ParseSource + the #342 module-install path
//     (catalog.InstallFromManifest + addServiceToManifestWithTags),
//     then `docker compose up` for the new service.
//   - Uninstall-> stop + remove the service from citadel.yaml / modules.lock.
//   - Start/Stop-> `docker compose up/down` for an already-installed service.
//   - ListInstalled -> read modules.lock (source/ref/commit/digests) joined
//     with the live container run-state and health.
//
// Every method takes a context so the live adapter can honor timeouts /
// cancellation from the reconcile loop.
type ModuleOps interface {
	// Install installs a module from the given assignment's Source with its
	// Config overrides. It is the convergence action for a missing module and
	// for an update (the engine uninstalls-then-installs, or the adapter may
	// implement an in-place update — either is acceptable as long as the
	// resulting ListInstalled reflects the new source/ref/config). After a
	// successful Install the module is expected to be RUNNING; the engine
	// issues a follow-up Stop when DesiredStatus is stopped.
	Install(ctx context.Context, m ModuleAssignment) error

	// Uninstall removes an installed module by name (stop + de-register).
	Uninstall(ctx context.Context, name string) error

	// Start brings an already-installed module up.
	Start(ctx context.Context, name string) error

	// Stop brings an already-installed module down (without uninstalling it).
	Stop(ctx context.Context, name string) error

	// ListInstalled returns the actual on-node state of every installed module.
	//
	// CRITICAL: each returned InstalledModule MUST carry enough to diff against
	// the desired state — at minimum Name, Source, Ref, Config, and a Health
	// that distinguishes running vs stopped. Without Source/Ref/Config the
	// engine cannot detect update-on-source-change or update-on-config-change,
	// and cannot prove idempotency.
	ListInstalled(ctx context.Context) ([]InstalledModule, error)
}
