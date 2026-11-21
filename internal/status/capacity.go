package status

import (
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

const capacityReadLimit = 4096

var errCapacityUnavailable = errors.New("capacity observation unavailable")

type capacityProbes struct {
	load     func() ([3]float64, error)
	ncpu     func() int
	pressure func(string) ([]byte, error)
}

// collectCapacityMetrics is fail-open per source: no malformed or absent
// optional observation can reject the heartbeat or fabricate a healthy zero.
func collectCapacityMetrics(p capacityProbes) SystemMetrics {
	var out SystemMetrics
	if p.ncpu != nil {
		if n := p.ncpu(); n > 0 {
			out.NCPU = &n
		}
	}
	if p.load != nil {
		if values, err := p.load(); err == nil && validCapacityNumber(values[0]) && validCapacityNumber(values[1]) && validCapacityNumber(values[2]) {
			out.Load1, out.Load5, out.Load15 = &values[0], &values[1], &values[2]
		}
	}
	if p.pressure != nil {
		for _, resource := range []string{"cpu", "io", "memory"} {
			raw, err := p.pressure(resource)
			if err != nil {
				continue
			}
			some, full := parseCapacityPressure(raw)
			switch resource {
			case "cpu":
				out.PSICPUSomeAvg60 = some
			case "io":
				out.PSIIOSomeAvg60, out.PSIIOFullAvg60 = some, full
			case "memory":
				out.PSIMemorySomeAvg60 = some
			}
		}
	}
	return out
}

func validCapacityNumber(value float64) bool {
	// The ordered comparison also rejects NaN (NaN >= 0 is false).
	return !math.IsInf(value, 0) && value >= 0
}

func parseCapacityLoad(raw []byte) ([3]float64, error) {
	var values [3]float64
	if len(raw) > capacityReadLimit {
		return values, errCapacityUnavailable
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 5 {
		return values, errCapacityUnavailable
	}
	for i := range values {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil || !validCapacityNumber(v) {
			return [3]float64{}, errCapacityUnavailable
		}
		values[i] = v
	}
	return values, nil
}

// Only the named avg60 field is used, never avg10, avg300 or cumulative total.
// Ambiguous duplicate records/fields fail to unknown independently per kind.
func parseCapacityPressure(raw []byte) (some, full *float64) {
	if len(raw) > capacityReadLimit {
		return nil, nil
	}
	seen := map[string]bool{}
	values := map[string]*float64{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" && fields[0] != "full" {
			continue
		}
		kind := fields[0]
		if seen[kind] {
			values[kind] = nil
			continue
		}
		seen[kind] = true
		found := false
		for _, field := range fields[1:] {
			key, text, ok := strings.Cut(field, "=")
			if !ok || key != "avg60" {
				continue
			}
			if found {
				values[kind] = nil
				break
			}
			found = true
			v, err := strconv.ParseFloat(text, 64)
			if err == nil && validCapacityNumber(v) && v <= 100 {
				values[kind] = &v
			}
		}
	}
	return values["some"], values["full"]
}

func readCapacityFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errCapacityUnavailable
	}
	defer f.Close()
	return readCapacityInput(f)
}

func readCapacityInput(reader io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, capacityReadLimit+1))
	if err != nil || len(raw) > capacityReadLimit {
		return nil, errCapacityUnavailable
	}
	return raw, nil
}

// No caller path enters these probes. Only fixed kernel telemetry files are
// read; a test substitutes the reader, not the production source names.
func procCapacityProbes(read func(string) ([]byte, error), ncpu func() int) capacityProbes {
	return capacityProbes{
		ncpu: ncpu,
		load: func() ([3]float64, error) {
			raw, err := read("/proc/loadavg")
			if err != nil {
				return [3]float64{}, errCapacityUnavailable
			}
			return parseCapacityLoad(raw)
		},
		pressure: func(resource string) ([]byte, error) {
			switch resource {
			case "cpu", "io", "memory":
				return read("/proc/pressure/" + resource)
			default:
				return nil, errCapacityUnavailable
			}
		},
	}
}
