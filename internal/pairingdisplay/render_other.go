//go:build !linux

package pairingdisplay

// P0's only rendering mechanism is the Linux virtual-terminal text console
// (design doc §9.1). macOS `osascript` / Windows toast rendering is P3,
// explicitly deferred — see docs/design-pairing-display.md §11/§14.
// unsupportedRenderer always fails closed to delivered:false so the backend
// falls through to its linked-device channels, exactly as it does today.

func newPlatformRenderer() Renderer {
	return unsupportedRenderer{}
}

type unsupportedRenderer struct{}

func (unsupportedRenderer) ResolveTarget() (target string, reason string, ok bool) {
	return "", "unsupported_os", false
}

func (unsupportedRenderer) Show(string, ShowRequest) RenderResult {
	return RenderResult{Reason: "unsupported_os"}
}

func (unsupportedRenderer) Clear(string, string) error {
	return nil
}

func (unsupportedRenderer) DetectSurfaces() []string {
	return nil
}
