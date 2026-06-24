//go:build windows

package power

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Execution-state flags for SetThreadExecutionState. We request CONTINUOUS so
// the state persists until cleared, plus SYSTEM_REQUIRED to keep the system
// awake. We do NOT request DISPLAY_REQUIRED — a headless node does not need the
// screen lit, only the system reachable.
const (
	esContinuous      = 0x80000000
	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002 //nolint:unused // documented intent; not used
)

// ACLineStatus values from GetSystemPowerStatus.
const (
	acLineOffline = 0
	acLineOnline  = 1
)

var (
	modkernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procSetThreadExecutionState = modkernel32.NewProc("SetThreadExecutionState")
	procGetSystemPowerStatus    = modkernel32.NewProc("GetSystemPowerStatus")
)

// systemPowerStatus mirrors the Win32 SYSTEM_POWER_STATUS struct.
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// winInhibitor holds a SetThreadExecutionState assertion. The state is cleared
// (reset to ES_CONTINUOUS alone) on Stop and is automatically dropped when the
// process exits, so the machine can never be left permanently awake.
type winInhibitor struct {
	mu     sync.Mutex
	active bool
}

// NewInhibitor returns a Windows sleep inhibitor backed by
// SetThreadExecutionState(ES_CONTINUOUS|ES_SYSTEM_REQUIRED).
func NewInhibitor() Inhibitor {
	return &winInhibitor{}
}

func setExecutionState(flags uint32) error {
	r, _, err := procSetThreadExecutionState.Call(uintptr(flags))
	if r == 0 {
		return err
	}
	return nil
}

func (w *winInhibitor) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active {
		return nil
	}
	if err := setExecutionState(esContinuous | esSystemRequired); err != nil {
		return err
	}
	w.active = true
	return nil
}

func (w *winInhibitor) Stop() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return nil
	}
	// Clearing back to ES_CONTINUOUS alone releases the system-required hold.
	err := setExecutionState(esContinuous)
	w.active = false
	return err
}

func (w *winInhibitor) Active() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
}

// DetectPowerSource reads ACLineStatus via GetSystemPowerStatus.
func DetectPowerSource() Source {
	var status systemPowerStatus
	r, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return SourceUnknown
	}
	switch status.ACLineStatus {
	case acLineOnline:
		return SourceAC
	case acLineOffline:
		return SourceBattery
	default:
		return SourceUnknown
	}
}
