package pairingdisplay

import "io"

// writeFrameOrClear writes frame to w and, on any write error (including a
// short write — io.Writer's contract requires a non-nil error whenever
// n < len(p), so an error check alone catches that case too), makes a
// best-effort attempt to write a clear frame in its place before returning
// the original error.
//
// This closes a gap review caught: Manager does not arm a TTL timer or
// write a crash marker for a non-delivered Show outcome (see manager.go's
// Show — a failed render has nothing worth tracking for TTL purposes), so
// without this, a partial write from a failing/flaky console device could
// leave a fragment of the code on the physical screen with NOTHING that
// would ever reclaim it — exactly what the TTL/crash-marker machinery in
// §12 exists to prevent for every other failure mode. Reclaiming it at the
// point of failure, rather than threading a new tracked-but-undelivered
// state through Manager, keeps that contract simple.
//
// Extracted from render_linux.go's consoleRenderer.Show so this
// failure-handling logic is unit-testable with a fake io.Writer — no real
// console/VT access needed.
func writeFrameOrClear(w io.Writer, frame string) error {
	if _, err := io.WriteString(w, frame); err != nil {
		// Best-effort: if this second write also fails there is nothing
		// further to try locally. The caller's "render_error" result is
		// what makes this fail closed regardless of whether the clear
		// attempt itself succeeded.
		_, _ = io.WriteString(w, renderClearFrame("display error, cleared"))
		return err
	}
	return nil
}
