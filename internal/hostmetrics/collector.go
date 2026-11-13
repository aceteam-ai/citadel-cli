// Package hostmetrics prepares the inactive S1 collector. It opens no listener
// and is deliberately not registered by a worker or heartbeat entrypoint.
package hostmetrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Family uint8

const (
	CPU Family = iota
	Memory
	Disk
	Network
	Temperature
	Throttle
	Process
	GPU
	Job
	familyCount
)

var families = [...]string{"cpu", "memory", "disk", "network", "temperature", "throttle", "process", "gpu", "job"}

type Metric uint8

const (
	CPUTicks Metric = iota
	CPUIdleTicks
	MemoryTotal
	MemoryAvailable
	DiskReadBytes
	DiskWriteBytes
	NetworkReceiveBytes
	NetworkTransmitBytes
	TemperatureCelsius
	ThrottleEvents
	ProcessTicks
	ProcessResidentBytes
	GPUUtilizationRatio
	GPUMemoryBytes
	JobActive
	JobCPUSeconds
	JobResidentBytes
	JobGeneration
)

type descriptor struct {
	name   string
	family Family
}

var descriptors = [...]descriptor{
	{"cpu_ticks_total", CPU}, {"cpu_idle_ticks_total", CPU},
	{"memory_total_bytes", Memory}, {"memory_available_bytes", Memory},
	{"disk_read_bytes_total", Disk}, {"disk_written_bytes_total", Disk},
	{"network_received_bytes_total", Network}, {"network_transmitted_bytes_total", Network},
	{"temperature_celsius", Temperature}, {"throttle_events_total", Throttle},
	{"process_cpu_ticks_total", Process}, {"process_resident_bytes", Process},
	{"gpu_utilization_ratio", GPU}, {"gpu_memory_used_bytes", GPU},
	{"job_active", Job}, {"job_cpu_seconds", Job}, {"job_resident_bytes", Job}, {"job_generation", Job},
}

// Key is private source identity, never a Prometheus label. A family-local slot
// is stable for this collector lifetime. No host names, interface names, paths,
// PIDs, job IDs, command lines, usernames or process arguments are exported.
type Reading struct {
	Metric Metric
	Key    string
	Value  float64
}
type Group struct {
	Known    bool
	Readings []Reading
}
type Snapshot struct{ Groups map[Family]Group }

// Source must honor ctx and the shared read budget, without background work.
type Source interface {
	Read(context.Context, *Budget) (Snapshot, error)
}

// Limits are mandatory source policy, not measured production defaults. The
// Phase A proof must ratify the chosen values before any adapter is activated.
type Limits struct {
	Timeout                                                                   time.Duration
	MaxReadBytes, MaxFileBytes, MaxFiles, MaxSlots, MaxSeries, MaxOutputBytes int
}

func (l Limits) validate() error {
	if l.Timeout <= 0 || l.Timeout > 5*time.Second || l.MaxReadBytes < 1 || l.MaxReadBytes > 4<<20 || l.MaxFileBytes < 1 || l.MaxFileBytes > 64<<10 || l.MaxFileBytes > l.MaxReadBytes || l.MaxFiles < 1 || l.MaxFiles > 128 || l.MaxSlots < 1 || l.MaxSlots > 64 || l.MaxSeries < int(familyCount) || l.MaxSeries > 4096 || l.MaxOutputBytes < 256 || l.MaxOutputBytes > 512<<10 {
		return errors.New("invalid metrics limits")
	}
	return nil
}

var nodeIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

type Collector struct {
	mu     sync.Mutex
	nodeID string
	limits Limits
	source Source
	slots  [familyCount]map[string]int
}

func New(nodeID string, limits Limits, source Source) (*Collector, error) {
	if !nodeIDPattern.MatchString(nodeID) || source == nil {
		return nil, errors.New("invalid metrics identity or source")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	c := &Collector{nodeID: nodeID, limits: limits, source: source}
	for i := range c.slots {
		c.slots[i] = map[string]int{}
	}
	return c, nil
}

// Collect refuses concurrent work and fails the entire scrape on truncation,
// timeout, invalid samples or capacity exhaustion. Missing families remain
// explicitly unknown; an absent sensor is never fabricated as a zero value.
func (c *Collector) Collect(parent context.Context) (string, error) {
	if !c.mu.TryLock() {
		return "", errors.New("metrics collection busy")
	}
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, c.limits.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	snapshot, err := c.source.Read(ctx, &Budget{limits: c.limits})
	if err != nil {
		return "", errors.New("metrics source unavailable")
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if len(snapshot.Groups) > int(familyCount) {
		return "", errors.New("invalid metrics families")
	}
	for family := range snapshot.Groups {
		if family >= familyCount {
			return "", errors.New("invalid metrics family")
		}
	}
	// Stage slot allocation so failed scrapes cannot consume identity capacity.
	var pending [familyCount]map[string]int
	for f, slots := range c.slots {
		pending[f] = make(map[string]int, len(slots))
		for k, v := range slots {
			pending[f][k] = v
		}
	}
	var lines []string
	for f := Family(0); f < familyCount; f++ {
		group := snapshot.Groups[f]
		known := 0
		if group.Known {
			known = 1
		}
		lines = append(lines, fmt.Sprintf("citadel_metrics_family_known{node_id=%q,family=%q} %d\n", c.nodeID, families[f], known))
		if !group.Known {
			if len(group.Readings) > 0 {
				return "", errors.New("unknown family contains values")
			}
			continue
		}
		if len(group.Readings) > c.limits.MaxSeries {
			return "", errors.New("metrics series capacity exceeded")
		}
		seen := map[string]bool{}
		for _, r := range group.Readings {
			if int(r.Metric) >= len(descriptors) || descriptors[r.Metric].family != f || len(r.Key) == 0 || len(r.Key) > 128 || math.IsNaN(r.Value) || math.IsInf(r.Value, 0) || (r.Value < 0 && r.Metric != TemperatureCelsius) || (r.Metric == GPUUtilizationRatio && r.Value > 1) {
				return "", errors.New("invalid metrics sample")
			}
			id := strconv.Itoa(int(r.Metric)) + ":" + r.Key
			if seen[id] {
				return "", errors.New("duplicate metrics sample")
			}
			seen[id] = true
			slot, ok := pending[f][r.Key]
			if !ok {
				if len(pending[f]) >= c.limits.MaxSlots {
					return "", errors.New("metrics identity capacity exceeded")
				}
				slot = len(pending[f])
				pending[f][r.Key] = slot
			}
			lines = append(lines, fmt.Sprintf("citadel_%s{node_id=%q,slot=%q} %s\n", descriptors[r.Metric].name, c.nodeID, strconv.Itoa(slot), strconv.FormatFloat(r.Value, 'g', -1, 64)))
			if len(lines) > c.limits.MaxSeries {
				return "", errors.New("metrics series capacity exceeded")
			}
		}
	}
	if len(lines) > c.limits.MaxSeries {
		return "", errors.New("metrics series capacity exceeded")
	}
	sort.Strings(lines)
	var out strings.Builder
	for _, line := range lines {
		if out.Len()+len(line) > c.limits.MaxOutputBytes {
			return "", errors.New("metrics output capacity exceeded")
		}
		out.WriteString(line)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.slots = pending
	return out.String(), nil
}
