//go:build !linux && !darwin && !windows

package update

import (
	"context"
	"fmt"
	"os"
)

type installedIdentity struct{}
type mutationLock struct{}
type fileIdentity struct{}

func acquireMutationLock(context.Context, string, bool) (*mutationLock, error) {
	return nil, fmt.Errorf("update lock unavailable: unsupported platform")
}

func acquireRecoveryMutationLock(context.Context, string) (*mutationLock, error) {
	return nil, fmt.Errorf("update recovery unsupported on this platform")
}

func (l *mutationLock) Release() error { return nil }

func setStageOwner(*os.File, installedIdentity) error {
	return fmt.Errorf("update transaction unsupported on this platform")
}

func createPrivateAttemptDir(string) (string, os.FileInfo, error) {
	return "", nil, fmt.Errorf("update attempt unsupported on this platform")
}

func validatePrivateAttemptDir(string) (os.FileInfo, error) {
	return nil, fmt.Errorf("update attempt unsupported on this platform")
}

func openRegularNoFollow(string) (*os.File, error) {
	return nil, fmt.Errorf("executable metadata unsupported on this platform")
}

func captureFileIdentity(*os.File) (fileIdentity, error) {
	return fileIdentity{}, fmt.Errorf("file identity unsupported on this platform")
}

func validateCapturedFileIdentity(*os.File, fileIdentity) error {
	return fmt.Errorf("file identity unsupported on this platform")
}
