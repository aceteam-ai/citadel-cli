package update

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

type ApplyIntent uint8

const (
	ExactRemote ApplyIntent = iota + 1
	Latest
)

type ApplyDisposition uint8

const (
	Applied ApplyDisposition = iota + 1
	AlreadyCurrent
	Superseded
)

type ApplyResult struct {
	Disposition     ApplyDisposition
	InstalledBefore string
	InstalledAfter  string
}

var resolveInstalledPath = GetCurrentBinaryPath
var beforeFinalInstalledCheck = func(string) {}
var preserveSwapStageOwner = setStageOwner

// CheckExactTarget performs the locked, installed-disk preflight for an exact
// remote update. It never holds the lock across release lookup or download.
func CheckExactTarget(ctx context.Context, target string) (string, VersionRelation, error) {
	canonical, err := NormalizeExactVersion(target)
	if err != nil {
		return "", 0, err
	}
	destination, err := resolveInstalledPath()
	if err != nil {
		return "", 0, fmt.Errorf("resolve installed Citadel executable: %w", err)
	}
	lock, err := acquireMutationLock(ctx, destination, true)
	if err != nil {
		return "", 0, err
	}
	defer lock.Release()
	meta, err := ReadExecutableVersion(destination)
	if err != nil {
		return "", 0, deterministicUpdateError("unknown_installed", "cannot verify installed Citadel release version; refusing exact update")
	}
	relation, err := CompareReleaseVersions(canonical, meta.Version)
	if err != nil {
		return "", 0, err
	}
	if relation == TargetOlder {
		return "", 0, deterministicUpdateError("stale_target", "exact update target is older than the installed Citadel release")
	}
	return meta.Version, relation, nil
}

// ApplyRelease binds requested, fetched, and completed-stage identity while
// holding the installed-path mutation lock through commit and rollback.
func ApplyRelease(ctx context.Context, candidatePath, requestedTarget, fetchedTag string, intent ApplyIntent) (ApplyResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var expected string
	var err error
	switch intent {
	case ExactRemote:
		expected, err = NormalizeExactVersion(requestedTarget)
		if err != nil {
			return ApplyResult{}, err
		}
		fetched, fetchErr := NormalizeExactVersion(fetchedTag)
		if fetchErr != nil || fetched != expected {
			return ApplyResult{}, deterministicUpdateError("release_mismatch", "fetched release does not match exact update target")
		}
	case Latest:
		expected, err = NormalizeExactVersion(fetchedTag)
		if err != nil {
			return ApplyResult{}, deterministicUpdateError("release_mismatch", "release has an invalid version identity")
		}
	default:
		return ApplyResult{}, fmt.Errorf("unknown update intent")
	}

	destination, err := resolveInstalledPath()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("resolve installed Citadel executable: %w", err)
	}
	if IsHomebrewManagedPath(destination, runtime.GOOS) {
		return ApplyResult{}, ErrHomebrewManaged
	}
	if IsDesktopManagedPath(destination, runtime.GOOS) {
		return ApplyResult{}, ErrDesktopManaged
	}
	lock, err := acquireMutationLock(ctx, destination, true)
	if err != nil {
		return ApplyResult{}, err
	}
	defer lock.Release()

	if runtime.GOOS == "windows" {
		if _, err := recoverInterruptedSwapLocked(destination, lock.identity); err != nil {
			return ApplyResult{}, err
		}
	}
	installed, err := ReadExecutableVersion(destination)
	if err != nil {
		return ApplyResult{}, installedMetadataError(intent)
	}
	relation, err := compareForIntent(expected, installed, intent)
	if err != nil {
		return ApplyResult{}, err
	}
	result := ApplyResult{InstalledBefore: installed.Version, InstalledAfter: installed.Version}
	if relation != TargetNewer {
		if intent == ExactRemote && relation == TargetOlder {
			return ApplyResult{}, deterministicUpdateError("stale_target", "exact update target is older than the installed Citadel release")
		}
		if relation == TargetEqual {
			result.Disposition = AlreadyCurrent
		} else {
			result.Disposition = Superseded
		}
		return result, nil
	}

	candidate, err := openRegularNoFollow(candidatePath)
	if err != nil {
		return ApplyResult{}, deterministicUpdateError("candidate_invalid", "candidate executable cannot be opened safely")
	}
	defer candidate.Close()
	stage, stagePath, err := createSwapStage(destination, lock.identity)
	if err != nil {
		return ApplyResult{}, err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			removeOwnedStage(stage, stagePath)
		}
		_ = stage.Close()
	}()
	if _, err := io.Copy(stage, candidate); err != nil {
		return ApplyResult{}, fmt.Errorf("copy candidate into swap stage: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := stage.Chmod(0o755); err != nil {
			return ApplyResult{}, fmt.Errorf("set swap stage mode: %w", err)
		}
	}
	if err := preserveSwapStageOwner(stage, lock.identity); err != nil {
		return ApplyResult{}, err
	}
	if err := stage.Sync(); err != nil {
		return ApplyResult{}, fmt.Errorf("sync swap stage: %w", err)
	}
	stageMeta, err := validateExecutableForInstall(stage)
	if err != nil {
		return ApplyResult{}, deterministicUpdateError("candidate_invalid", "candidate executable metadata is invalid or incompatible")
	}
	stageVersion, err := NormalizeExactVersion(stageMeta.Version)
	if err != nil || stageVersion != expected {
		return ApplyResult{}, deterministicUpdateError("candidate_mismatch", "candidate executable does not match requested release")
	}
	if err := validateOpenPathIdentity(stage, stagePath); err != nil {
		return ApplyResult{}, fmt.Errorf("swap stage identity changed: %w", err)
	}
	beforeFinalInstalledCheck(stagePath)
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	finalInstalled, err := ReadExecutableVersion(destination)
	if err != nil {
		return ApplyResult{}, installedMetadataError(intent)
	}
	finalRelation, err := compareForIntent(expected, finalInstalled, intent)
	if err != nil {
		return ApplyResult{}, err
	}
	result.InstalledBefore = finalInstalled.Version
	result.InstalledAfter = finalInstalled.Version
	if finalRelation != TargetNewer {
		if intent == ExactRemote && finalRelation == TargetOlder {
			return ApplyResult{}, deterministicUpdateError("stale_target", "exact update target is older than the installed Citadel release")
		}
		if finalRelation == TargetEqual {
			result.Disposition = AlreadyCurrent
		} else {
			result.Disposition = Superseded
		}
		return result, nil
	}
	if err := validateOpenPathIdentity(stage, stagePath); err != nil {
		return ApplyResult{}, fmt.Errorf("swap stage identity changed before commit: %w", err)
	}
	if err := backupCurrentLocked(destination); err != nil {
		return ApplyResult{}, fmt.Errorf("backup installed executable: %w", err)
	}
	if err := replaceValidatedStageLocked(stage, stagePath, destination, lock.identity, expected); err != nil {
		if rollbackErr := rollbackLocked(destination, lock.identity); rollbackErr != nil {
			return ApplyResult{}, fmt.Errorf("replace failed (%w) and rollback failed (%v)", err, rollbackErr)
		}
		return ApplyResult{}, fmt.Errorf("replace failed, rolled back: %w", err)
	}
	keepStage = runtime.GOOS == "windows"
	installedAfter, err := ReadExecutableVersion(destination)
	if err != nil || !ExactVersionIdentityEqual(installedAfter.Version, expected) {
		if rollbackErr := rollbackLocked(destination, lock.identity); rollbackErr != nil {
			return ApplyResult{}, fmt.Errorf("post-install validation failed and rollback failed: %v", rollbackErr)
		}
		return ApplyResult{}, fmt.Errorf("post-install validation failed; rolled back")
	}
	result.Disposition = Applied
	result.InstalledAfter = installedAfter.Version
	return result, nil
}

func installedMetadataError(intent ApplyIntent) error {
	if intent == ExactRemote {
		return deterministicUpdateError("unknown_installed", "cannot verify installed Citadel release version; refusing exact update")
	}
	return fmt.Errorf("cannot verify installed Citadel release version")
}

func compareForIntent(target string, installed ExecutableVersion, intent ApplyIntent) (VersionRelation, error) {
	if installed.Source == VersionSourceDevelopment && installed.Version == "dev" {
		if intent == Latest {
			return TargetNewer, nil
		}
		return 0, deterministicUpdateError("unknown_installed", "cannot verify installed Citadel release version; refusing exact update")
	}
	return CompareReleaseVersions(target, installed.Version)
}

func createSwapStage(destination string, identity installedIdentity) (*os.File, string, error) {
	file, err := os.CreateTemp(filepath.Dir(destination), ".citadel-update-stage-")
	if err != nil {
		return nil, "", fmt.Errorf("create same-directory swap stage: %w", err)
	}
	if err := preserveSwapStageOwner(file, identity); err != nil {
		path := file.Name()
		_ = file.Close()
		_ = os.Remove(path)
		return nil, "", err
	}
	return file, file.Name(), nil
}

func validateOpenPathIdentity(file *os.File, path string) error {
	held, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if named.Mode()&os.ModeSymlink != 0 || !os.SameFile(held, named) {
		return fmt.Errorf("path no longer names held file")
	}
	return nil
}

func removeOwnedStage(file *os.File, path string) {
	if err := validateOpenPathIdentity(file, path); err != nil {
		return
	}
	_ = os.Remove(path)
}

func backupCurrentLocked(destination string) error {
	if err := EnsureUpdateDir(); err != nil {
		return err
	}
	source, err := openRegularNoFollow(destination)
	if err != nil {
		return err
	}
	defer source.Close()
	stage, err := os.CreateTemp(GetUpdateDir(), ".previous-")
	if err != nil {
		return err
	}
	stagePath := stage.Name()
	defer func() {
		_ = stage.Close()
		_ = os.Remove(stagePath)
	}()
	if _, err := io.Copy(stage, source); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := stage.Chmod(0o755); err != nil {
			return err
		}
	}
	if err := stage.Sync(); err != nil {
		return err
	}
	if err := stage.Close(); err != nil {
		return err
	}
	previous := GetPreviousBinaryPath()
	if runtime.GOOS == "windows" {
		if err := os.Remove(previous); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(stagePath, previous)
}

func replaceValidatedStageLocked(stage *os.File, stagePath, destination string, identity installedIdentity, expectedVersion string) error {
	if err := validateOpenPathIdentity(stage, stagePath); err != nil {
		return err
	}
	stageIdentity, err := captureFileIdentity(stage)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Rename(stagePath, destination)
	}
	if err := stage.Close(); err != nil {
		return err
	}
	newPath := destination + ".new"
	oldPath := destination + ".old"
	if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stagePath, newPath); err != nil {
		return err
	}
	if err := validateReservedWindowsStage(newPath, stageIdentity, identity, expectedVersion); err != nil {
		return err
	}
	if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Reopen through the reserved name immediately before moving the original
	// destination. This is the strongest available check after closing the
	// stage handle for Windows rename semantics.
	if err := validateReservedWindowsStage(newPath, stageIdentity, identity, expectedVersion); err != nil {
		return err
	}
	if err := os.Rename(destination, oldPath); err != nil {
		return err
	}
	if err := os.Rename(newPath, destination); err != nil {
		_ = os.Rename(oldPath, destination)
		return err
	}
	return nil
}

func validateReservedWindowsStage(path string, original fileIdentity, owner installedIdentity, expectedVersion string) error {
	file, err := openRegularNoFollow(path)
	if err != nil {
		return err
	}
	defer file.Close()
	meta, metadataErr := validateExecutableForInstall(file)
	pathErr := validateOpenPathIdentity(file, path)
	identityErr := validateCapturedFileIdentity(file, original)
	ownerErr := setStageOwner(file, owner)
	if metadataErr != nil || pathErr != nil || identityErr != nil || ownerErr != nil || !versionIdentityMatches(meta.Version, expectedVersion) {
		return fmt.Errorf("reserved Windows swap stage failed validation")
	}
	return nil
}

func rollbackLocked(destination string, identity installedIdentity) error {
	previous := GetPreviousBinaryPath()
	if _, err := os.Stat(previous); err != nil {
		return err
	}
	stage, stagePath, err := createSwapStage(destination, identity)
	if err != nil {
		return err
	}
	defer func() {
		removeOwnedStage(stage, stagePath)
		_ = stage.Close()
	}()
	src, err := openRegularNoFollow(previous)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(stage, src)
	_ = src.Close()
	if copyErr != nil {
		return copyErr
	}
	if runtime.GOOS != "windows" {
		if err := stage.Chmod(0o755); err != nil {
			return err
		}
	}
	if err := stage.Sync(); err != nil {
		return err
	}
	previousMeta, err := validateExecutableForInstall(stage)
	if err != nil {
		return err
	}
	return replaceValidatedStageLocked(stage, stagePath, destination, identity, previousMeta.Version)
}

func versionIdentityMatches(actual, expected string) bool {
	if actual == "dev" || expected == "dev" {
		return actual == expected
	}
	return ExactVersionIdentityEqual(actual, expected)
}

func validateOwnerAuthority(expected, actual string) error {
	if expected == "" || actual == "" || expected != actual {
		return fmt.Errorf("stage owner does not match installed executable")
	}
	return nil
}
