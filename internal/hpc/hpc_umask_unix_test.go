//go:build !windows

package hpc

import "syscall"

// The hpc profile refuses a state base that group or other can write without
// the sticky bit. A developer umask of 002 makes every t.TempDir 0775, so the
// test binary runs with a private umask to keep fixtures realistic.
func init() { syscall.Umask(0o077) }
