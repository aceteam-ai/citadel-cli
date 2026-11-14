//go:build windows

package cobrowserecovery

import "os"

func Supported() bool                            { return false }
func privateDir(string) (bool, error)            { return false, ErrUnavailable }
func privateOpen(string, bool) (*os.File, error) { return nil, ErrUnavailable }
func lockExclusive(*os.File) error               { return ErrUnavailable }

func checkPrivateDir(string) error { return ErrUnavailable }
