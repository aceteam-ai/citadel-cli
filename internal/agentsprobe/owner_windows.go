//go:build windows

// internal/agentsprobe/owner_windows.go
//
// Windows stub: there is no numeric uid on the node config dir to key on, so the
// node-dir-owner tier is never available and the resolver falls through to the
// process user (Target.Signal == process), as the S2 design specifies.
package agentsprobe

func statOwnerUID(_ string) (int, bool) { return 0, false }
