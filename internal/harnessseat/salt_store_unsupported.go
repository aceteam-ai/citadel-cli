//go:build !linux && !darwin

package harnessseat

import (
	"context"
	"os"
)

// Until native privacy/locking checks exist, refuse rather than claim that
// POSIX mode bits prove private ownership on another platform.
func privateStoreSupported() bool                      { return false }
func privateNoFollowFlags() int                        { return 0 }
func privateStoreInfo(os.FileInfo, bool) bool          { return false }
func privateStoreLock(context.Context, *os.File) error { return ErrStoreUnsupported }
func privateStoreUnlock(*os.File)                      {}
