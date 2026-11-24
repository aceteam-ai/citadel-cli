//go:build !windows

package localcobrowse

func uploadRenameRetryable(error) bool { return false }
