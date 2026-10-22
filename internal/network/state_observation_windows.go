//go:build windows

package network

import "os"

func openStateObservation(path string) (*os.File, error) {
	return os.Open(path)
}

func stateObservationConnectionRefused(error) bool { return false }
