//go:build !darwin && !linux && !windows

package power

// noopInhibitor is used on platforms with no supported sleep-inhibition
// mechanism. Start/Stop are no-ops, so keep-awake silently does nothing.
type noopInhibitor struct{}

func (noopInhibitor) Start() error { return nil }
func (noopInhibitor) Stop() error  { return nil }
func (noopInhibitor) Active() bool { return false }

// NewInhibitor returns a no-op inhibitor on unsupported platforms.
func NewInhibitor() Inhibitor { return noopInhibitor{} }

// DetectPowerSource cannot determine power state on unsupported platforms.
func DetectPowerSource() Source { return SourceUnknown }
