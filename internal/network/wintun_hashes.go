// internal/network/wintun_hashes.go
// SHA-256 pins for the embedded wintun.dll bytes (issue #709).
//
// Deliberately NOT build-tagged: internal/network/wintun_embed_windows.go
// embeds the actual DLL bytes behind //go:build windows so non-Windows
// binaries carry none of it, but that also means a hash-pin test gated the
// same way would only ever run on a Windows CI runner, which this repo's CI
// does not have (cross-compile only, see .github/workflows/ci.yml). Keeping
// the pins here, unconstrained, lets wintun_hashes_test.go verify the
// checked-in winassets/*.dll files against these hashes on every `go test
// ./...` run regardless of host OS.
package network

// Source: wintun-0.14.1.zip from https://www.wintun.net/builds/wintun-0.14.1.zip
// (sha256 07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51 for the
// zip itself). The two DLLs below are the unmodified amd64 and arm64
// wintun.dll payloads extracted from that archive's bin/<arch>/wintun.dll —
// the Prebuilt Binaries License (winassets/LICENSE.txt) §3a forbids
// modifying them, so these hashes pin the exact bytes citadel ships.
const (
	wintunAMD64SHA256 = "e5da8447dc2c320edc0fc52fa01885c103de8c118481f683643cacc3220dafce"
	wintunARM64SHA256 = "f7ba89005544be9d85231a9e0d5f23b2d15b3311667e2dad0debd344918a3f80"
)

// wintunAssetPaths maps GOARCH to the checked-in asset file, relative to this
// package's directory. Used by both the embed directives (which need a
// literal path) and the cross-platform hash-pin test (which needs a path it
// can os.ReadFile without relying on go:embed being active).
var wintunAssetPaths = map[string]string{
	"amd64": "winassets/wintun_amd64.dll",
	"arm64": "winassets/wintun_arm64.dll",
}

var wintunHashPins = map[string]string{
	"amd64": wintunAMD64SHA256,
	"arm64": wintunARM64SHA256,
}
