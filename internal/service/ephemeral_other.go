//go:build !linux

package service

func EphemeralManagedExecStarts() []string { return nil }
