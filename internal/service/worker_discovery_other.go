//go:build !linux

package service

func DiscoverActiveWorkers(string) ([]ManagedWorker, error) { return nil, nil }
