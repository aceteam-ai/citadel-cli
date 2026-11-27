//go:build windows

package laneop

import "errors"

// LANE_OPERATION is not offered on Windows: there is no supported tmux there,
// so the ledger refuses rather than running without a cross-process lock.
var errUnsupportedPlatform = errors.New("lane operations are not supported on Windows")

func lockFile(string) (func(), error) { return nil, errUnsupportedPlatform }

func syncDir(string) error { return errUnsupportedPlatform }
