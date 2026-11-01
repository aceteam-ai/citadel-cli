package containerjobs

import "math"

const maxHostEntries = 4096

// HostLimits is an explicit owner-local budget. Zero values grant nothing.
// Neither APPLY_DEVICE_CONFIG nor a container request can set these ceilings.
type HostLimits struct {
	MaxQuotaBytes  int64
	MaxVolumes     int
	MaxMemoryBytes int64
	MaxPids        int64
	MaxCPUs        float64
	MinFreeBytes   int64
}

func (l HostLimits) Valid() bool {
	return validQuota(l.MaxQuotaBytes) && l.MaxVolumes > 0 && l.MaxVolumes <= maxHostEntries &&
		l.MaxMemoryBytes > 0 && l.MaxPids > 0 && l.MaxPids <= math.MaxInt32 &&
		l.MaxCPUs > 0 && !math.IsNaN(l.MaxCPUs) && !math.IsInf(l.MaxCPUs, 0) && l.MinFreeBytes > 0
}

func (l HostLimits) checkRequest(r Request, compute bool) error {
	if !l.Valid() {
		return ErrUnavailable
	}
	if !validQuota(r.QuotaBytes) || r.QuotaBytes > l.MaxQuotaBytes {
		return ErrBounds
	}
	if compute && (r.MemoryBytes <= 0 || r.MemoryBytes > l.MaxMemoryBytes || r.PidsLimit <= 0 || r.PidsLimit > l.MaxPids || r.CPUs <= 0 || math.IsNaN(r.CPUs) || math.IsInf(r.CPUs, 0) || r.CPUs > l.MaxCPUs) {
		return ErrBounds
	}
	return nil
}

func (l HostLimits) checkCapacity(quota int64, available uint64, volumes int) error {
	if !l.Valid() {
		return ErrUnavailable
	}
	if !validQuota(quota) || quota > l.MaxQuotaBytes || volumes < 0 || volumes >= l.MaxVolumes {
		return ErrBounds
	}
	// Subtract only after checking, so even MaxInt64 budgets cannot overflow.
	if uint64(quota) > available || uint64(l.MinFreeBytes) > available-uint64(quota) {
		return ErrBounds
	}
	return nil
}
