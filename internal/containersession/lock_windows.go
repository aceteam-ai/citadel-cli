//go:build windows

package containersession

import "errors"

func acquireLock(string) (func(), error) {
	return nil, errors.New("container sessions are not supported on windows")
}
