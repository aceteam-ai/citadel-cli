package containerjobs

import (
	"math"
	"testing"
)

func fixtureHostLimits() HostLimits {
	return HostLimits{MaxQuotaBytes: 10 << 30, MaxVolumes: 4, MaxMemoryBytes: 2 << 30, MaxPids: 256, MaxCPUs: 1, MinFreeBytes: 2 << 30}
}
func TestContainerHostLimitsRefuseMissingAndExcessBudgets(t *testing.T) {
	l := fixtureHostLimits()
	r := baseRequest()
	r.QuotaBytes = 16 << 20
	r.MemoryBytes = 2 << 30
	r.PidsLimit = 256
	r.CPUs = 1
	if l.checkRequest(r, true) != nil || l.checkCapacity(r.QuotaBytes, 4<<30, 0) != nil {
		t.Fatal("bounded positive control refused")
	}
	for _, change := range []func(*HostLimits){func(p *HostLimits) { p.MaxQuotaBytes = 0 }, func(p *HostLimits) { p.MaxVolumes = 0 }, func(p *HostLimits) { p.MaxVolumes = maxHostEntries + 1 }, func(p *HostLimits) { p.MaxMemoryBytes = 0 }, func(p *HostLimits) { p.MaxPids = 0 }, func(p *HostLimits) { p.MaxCPUs = 0 }, func(p *HostLimits) { p.MaxCPUs = math.Inf(1) }, func(p *HostLimits) { p.MinFreeBytes = 0 }} {
		p := l
		change(&p)
		if p.Valid() || p.checkRequest(r, true) == nil || p.checkCapacity(r.QuotaBytes, 4<<30, 0) == nil {
			t.Fatal("missing/invalid local limit granted resources")
		}
	}
	for _, change := range []func(*Request){func(r *Request) { r.QuotaBytes = 11 << 30 }, func(r *Request) { r.MemoryBytes++ }, func(r *Request) { r.PidsLimit++ }, func(r *Request) { r.CPUs = 2 }} {
		q := r
		change(&q)
		if l.checkRequest(q, true) == nil {
			t.Fatal("request exceeded owner-local ceiling")
		}
	}
	for _, c := range []struct {
		quota int64
		free  uint64
		count int
	}{{16 << 20, 4 << 30, 4}, {16 << 20, 16 << 20, 0}, {16 << 20, (2 << 30) + (16 << 20) - 1, 0}, {11 << 30, 20 << 30, 0}, {math.MaxInt64, math.MaxUint64, 0}} {
		if l.checkCapacity(c.quota, c.free, c.count) == nil {
			t.Fatal("free space, count or quota guard missing")
		}
	}
	if maxHostEntries != 4096 {
		t.Fatal("host entry scan bound changed without review")
	}
}
