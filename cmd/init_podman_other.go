//go:build !linux

package cmd

func installRootlessPodmanLinux() error      { return nil }
func configureNvidiaCDILinux() error         { return nil }
func finishProvisionedLinuxUserWorker() bool { return false }
