//go:build !linux && !darwin

package nodevault

import "os"

// Windows retains the existing node-directory ACL authority; Unix uid/gid
// inheritance is not a portable substitute for Windows ACL custody.
func prepareLockoutStateFile(*os.File, string) error { return nil }
