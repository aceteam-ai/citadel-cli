//go:build windows

// internal/network/wintun_extract_windows.go
// Extraction, locked-handle hash verification, and pre-load of the embedded
// wintun driver for machine-wide (TUN) mode (issue #709, the shipping piece
// of #643's Windows slice).
package network

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/windows"
)

// wintunFileName is the exact base name wintun's own loader asks for:
// golang.zx2c4.com/wintun's wintun.go does
// `modwintun = newLazyDLL("wintun.dll", setupLogger)`, and that loader only
// searches the running executable's own directory and System32 (see
// docs/machine-wide-tun.md). The extracted file must be named exactly this
// and live beside citadel.exe for LOAD_LIBRARY_SEARCH_APPLICATION_DIR to
// find it.
const wintunFileName = "wintun.dll"

// EnsureWintunDriver extracts, verifies, and pre-loads the embedded wintun
// driver. Call it before anything touches tstun/wintun -- both
// PreflightMachineWide and ConnectMachineWide's Windows callers do, via
// ensureWintunDriverIfNeeded in machinewide.go.
//
// Order matters and follows docs/machine-wide-tun.md exactly:
//  1. Resolve the running executable's REAL directory (symlinks resolved),
//     since that is the only place wintun's loader will find a DLL we drop.
//  2. Refuse if any non-admin principal can write there. This has to
//     happen BEFORE extraction: a verified hash on disk means nothing if
//     the directory -- or citadel.exe itself, sitting right next to it --
//     is attacker-writable, because the executable could simply be
//     replaced instead. See checkDirectoryNotWritableByNonAdmin.
//  3. Extract to <exeDir>/wintun.dll, verifying its hash under an exclusive
//     handle, then close that handle and pre-load the DLL. The load cannot
//     hold the write handle -- image mapping needs FILE_EXECUTE, which a live
//     write handle blocks (ERROR_SHARING_VIOLATION) -- and it does not need
//     to: step 2 already proved only admins can write the directory, so the
//     brief window between hash and load is not reachable by an unprivileged
//     racer. Windows caches loaded modules by resolved path, so wintun's own
//     later LoadLibraryEx("wintun.dll", search flags) resolves to the
//     identical already-loaded module rather than reading the file again.
//
// Re-extracts on every call rather than trusting whatever is already on
// disk from a previous run.
func EnsureWintunDriver() error {
	exeDir, err := executableDir()
	if err != nil {
		return fmt.Errorf("machine-wide mode: could not resolve citadel's own directory: %w", err)
	}

	if err := checkDirectoryNotWritableByNonAdmin(exeDir); err != nil {
		return err
	}

	data, wantHashHex, err := embeddedWintun(runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("machine-wide mode: %w", err)
	}

	destPath := filepath.Join(exeDir, wintunFileName)
	if err := extractAndPreload(destPath, data, wantHashHex); err != nil {
		return fmt.Errorf("machine-wide mode: failed to prepare the network driver: %w", err)
	}
	return nil
}

// executableDir resolves the directory containing the running executable,
// following symlinks so both the ACL check and the extraction target the
// real on-disk location.
func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(resolved), nil
}

// extractAndPreload writes+verifies the driver under an exclusive handle, then
// CLOSES that handle before loading the DLL.
//
// The write handle MUST be closed before the load. LoadLibraryEx maps the file
// as an executable image section, which opens it with FILE_EXECUTE access and a
// share mode that does not permit another handle's write access -- so a
// still-open GENERIC_WRITE handle (even one that shares FILE_SHARE_READ) makes
// the load fail with ERROR_SHARING_VIOLATION ("the process cannot access the
// file because it is being used by another process"). An earlier version held
// the handle open across the load on the assumption that FILE_SHARE_READ was
// enough because "the loader only needs to read the file" -- image mapping needs
// execute, not just read, so that assumption was wrong and machine-wide mode
// could never load its own extracted wintun.dll (verified on a real Win11 host).
//
// Closing before loading does open a window in which the on-disk bytes could in
// principle be swapped between the hash check and the load, but that is safe
// here: EnsureWintunDriver already ran checkDirectoryNotWritableByNonAdmin, so
// only administrators can write this directory, and an admin able to write
// %ProgramFiles%\Citadel is already trusted (they could replace citadel.exe
// itself). The exclusive handle still guards the write<->hash window, which is
// the part an unprivileged racer could otherwise reach.
func extractAndPreload(destPath string, data []byte, wantHashHex string) error {
	if err := writeAndVerifyWintun(destPath, data, wantHashHex); err != nil {
		return err
	}

	// flags pin dependency-DLL resolution to System32 + destPath's own
	// directory. flags=0 would leave resolution on the legacy search order,
	// which still consults the current working directory and PATH under
	// SafeDllSearchMode -- and EnsureWintunDriver's caller adds the install
	// dir to the Machine PATH, so an unpinned load could pick up a
	// same-named dependency planted there instead of System32's.
	//
	// Pre-loading here means wintun's own later LoadLibraryEx("wintun.dll", ...)
	// resolves to this already-cached module by path instead of reading the
	// file again.
	const loadFlags = windows.LOAD_LIBRARY_SEARCH_SYSTEM32 | windows.LOAD_LIBRARY_SEARCH_APPLICATION_DIR
	if _, err := windows.LoadLibraryEx(destPath, 0, loadFlags); err != nil {
		return fmt.Errorf("load %s: %w", destPath, err)
	}
	return nil
}

// writeAndVerifyWintun writes data to destPath and verifies its sha256 while
// holding an exclusive handle (GENERIC_READ|GENERIC_WRITE, share FILE_SHARE_READ
// only -- no write/delete sharing), so nothing can alter the bytes between the
// write and the hash check. The handle is closed when this function returns,
// which is required before the caller loads the file -- see extractAndPreload.
func writeAndVerifyWintun(destPath string, data []byte, wantHashHex string) error {
	pathPtr, err := windows.UTF16PtrFromString(destPath)
	if err != nil {
		return fmt.Errorf("encode path: %w", err)
	}

	h, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ, // deliberately NOT FILE_SHARE_WRITE / FILE_SHARE_DELETE
		nil,
		windows.CREATE_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	f := os.NewFile(uintptr(h), destPath)
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", destPath, err)
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("hash %s: %w", destPath, err)
	}
	if gotHashHex := hex.EncodeToString(hasher.Sum(nil)); gotHashHex != wantHashHex {
		return fmt.Errorf("%s: hash mismatch after write (got %s, want %s) -- the embedded driver may be corrupt; reinstall citadel", destPath, gotHashHex, wantHashHex)
	}
	return nil
}
