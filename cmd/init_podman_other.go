//go:build !linux

package cmd

func installRootlessPodmanLinux() error          { return nil }
func configureNvidiaCDILinux() error             { return nil }
func finishProvisionedLinuxUserWorker() bool     { return false }
func hasNvidiaHardwareLinux() bool               { return false }
func isJetsonLinux() bool                        { return false }
func prepareLinuxPodmanProvision() (bool, error) { return false, nil }
func ensureDedicatedPodmanUser() error           { return nil }
func ownProvisionedLinuxState(string) error      { return nil }
