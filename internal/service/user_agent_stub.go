//go:build !darwin

package service

import "errors"

// errNoLaunchAgent is returned where there is no launchd.
var errNoLaunchAgent = errors.New("the LaunchAgent worker is only used on macOS")

// NewUserLaunchAgent is macOS-only.
func NewUserLaunchAgent() (*LaunchAgent, error) { return nil, errNoLaunchAgent }

// StartUserWorker is macOS-only.
func StartUserWorker() error { return errNoLaunchAgent }

// StopUserWorker is macOS-only.
func StopUserWorker() error { return errNoLaunchAgent }

// ReadUserAgentState is macOS-only.
func ReadUserAgentState(thisExe string) (LaunchAgentState, error) {
	return LaunchAgentState{}, errNoLaunchAgent
}
