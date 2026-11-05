//go:build !linux && !darwin && !windows

package update

import "path/filepath"

func inspectSelfUpdatePath(destination string) SelfUpdateCapability {
	return SelfUpdateCapability{State: SelfUpdateUnknown, Directory: filepath.Dir(destination)}
}
