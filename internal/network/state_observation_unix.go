//go:build unix

package network

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openStateObservation(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func stateObservationConnectionRefused(err error) bool {
	return errors.Is(err, unix.ECONNREFUSED)
}
