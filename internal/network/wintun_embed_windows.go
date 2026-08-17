//go:build windows

// internal/network/wintun_embed_windows.go
// Embeds the official prebuilt wintun.dll for amd64 and arm64 behind a
// Windows-only build tag, so a Linux or macOS citadel binary carries none of
// it (issue #709, the shipping piece of #643's machine-wide TUN mode).
//
// wintun's own Go loader (golang.zx2c4.com/wintun) is not a general DLL
// search: it calls
//
//	windows.LoadLibraryEx("wintun.dll", 0,
//	    LOAD_LIBRARY_SEARCH_APPLICATION_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32)
//
// so the only place an embedded copy can be extracted to is the running
// executable's own directory (or System32, which we do not write to). See
// wintun_extract_windows.go for the extraction, locked-handle hash
// verification, and pre-load that makes that safe.
package network

import (
	"fmt"

	_ "embed"
)

//go:embed winassets/wintun_amd64.dll
var wintunDLLAMD64 []byte

//go:embed winassets/wintun_arm64.dll
var wintunDLLARM64 []byte

// embeddedWintun returns the embedded driver bytes and pinned SHA-256 for
// goarch (runtime.GOARCH), or an error naming the unsupported architecture.
// citadel only ships windows/amd64 and windows/arm64 builds (see
// .github/workflows/ci.yml's cross-compile matrix); any other GOARCH has no
// embedded driver.
func embeddedWintun(goarch string) (data []byte, sha256Hex string, err error) {
	switch goarch {
	case "amd64":
		return wintunDLLAMD64, wintunAMD64SHA256, nil
	case "arm64":
		return wintunDLLARM64, wintunARM64SHA256, nil
	default:
		return nil, "", fmt.Errorf("no embedded wintun.dll for windows/%s (only amd64 and arm64 ship one)", goarch)
	}
}
