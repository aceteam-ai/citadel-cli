package service

// ManagedWorker is an observed active worker, not a permission grant. User is
// the unit's configured account when its literal spelling is known.
type ManagedWorker struct {
	AutoUpdateDisabled bool
	Unit               ManagedUnit
	Executable         string
	User               string
}
