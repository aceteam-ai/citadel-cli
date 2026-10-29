//go:build !linux && !darwin && !windows

package update

import (
	"fmt"
	"os"
)

func openStateLock(string) (*os.File, error) {
	return nil, fmt.Errorf("update state locking unsupported on this platform")
}
func tryStateLock(*os.File) (bool, error) {
	return false, fmt.Errorf("update state locking unsupported on this platform")
}
func unlockStateFile(*os.File) {}
func replaceStateFile(string, string) error {
	return fmt.Errorf("atomic update state unsupported on this platform")
}
func syncStateDir(string) error {
	return fmt.Errorf("update state durability unsupported on this platform")
}
func keepStateOwner(*os.File, string) error {
	return fmt.Errorf("update state ownership unsupported on this platform")
}
