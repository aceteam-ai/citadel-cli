//go:build !windows

// internal/network/machine_dir_trust_other.go
package network

// checkMachineDirACL is nil off Windows: the machine config dir there needs
// root to create.
var checkMachineDirACL func(string) error
