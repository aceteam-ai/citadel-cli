//go:build !unix && !windows

package network

import "os"

// Without a nonblocking descriptor primitive, do not risk opening a replaced
// special file. A caller receives unknown rather than fallback authority.
func openStateObservation(string) (*os.File, error) { return nil, errStateObservation }

func stateObservationConnectionRefused(error) bool { return false }
