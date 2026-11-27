//go:build windows

package cmd

import "errors"

// Local Flow container runs are unavailable without a durable run-claim lock.
func lockLocalFlowRunLedger(string) (func(), error) {
	return nil, errors.New("local Flow run ledger locking is unavailable on Windows")
}

func syncLocalFlowRunDir(string) error {
	return errors.New("local Flow run ledger syncing is unavailable on Windows")
}
