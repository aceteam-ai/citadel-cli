//go:build !linux

package main

import (
	"errors"
	"os"
)

func openPrivateConfigDir(string) (*os.File, error) {
	return nil, errors.New("nvrconfig requires its Linux init container")
}

func writePrivateConfig(*os.File, []byte) error {
	return errors.New("nvrconfig requires its Linux init container")
}
