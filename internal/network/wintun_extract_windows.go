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
//  3. Extract to <exeDir>/wintun.dll and pre-load it while still holding a
//     handle that denies write/delete sharing, so there is no gap between
//     "we hashed these exact bytes" and "the OS loaded them" for anything
//     to swap the file in (TOCTOU). Windows caches loaded modules by
//     resolved path, so wintun's own later
//     LoadLibraryEx("wintun.dll", search flags) resolves to the identical
//     already-loaded module rather than reading the file again.
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

// extractAndPreload writes data to destPath and loads it as a DLL through a
// single file handle held open, continuously, from creation through the
// load call -- opened with a share mode that denies write and delete to
// every other handle (FILE_SHARE_READ only). That is what makes "hash the
// bytes we just wrote, then load them" race-free: nothing else can touch
// destPath in between, because for the duration nothing else is allowed to.
// FILE_SHARE_READ is still granted because the OS loader itself needs to
// open the file for reading to map it when we call LoadLibraryEx below.
func extractAndPreload(destPath string, data []byte, wantHashHex string) error {
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
	gotHashHex := hex.EncodeToString(hasher.Sum(nil))
	if gotHashHex != wantHashHex {
		return fmt.Errorf("%s: hash mismatch after write (got %s, want %s) -- the embedded driver may be corrupt; reinstall citadel", destPath, gotHashHex, wantHashHex)
	}

	// Pre-load while STILL HOLDING the exclusive handle opened above: the
	// bytes on disk are guaranteed unchanged since the hash check, and
	// wintun's own later LoadLibraryEx("wintun.dll", ...) resolves to this
	// same cached module by path instead of reading the file again.
	if _, err := windows.LoadLibraryEx(destPath, 0, 0); err != nil {
		return fmt.Errorf("load %s: %w", destPath, err)
	}
	return nil
}
