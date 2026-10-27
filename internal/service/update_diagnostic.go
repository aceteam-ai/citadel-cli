package service

// WorkerUpdateDiagnostic is read-only evidence about a managed worker's
// install layout. It is advisory: ACLs, capabilities, mounts and the unit's
// filesystem namespace can differ from the CLI's view. Only the worker-side
// preflight can establish effective update authority.
type WorkerUpdateDiagnostic struct {
	Unit, WorkerUser, BinaryOwner, Directory string
	Warning                                  bool
	Detail                                   string
}
