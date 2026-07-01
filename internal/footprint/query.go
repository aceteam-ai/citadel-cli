package footprint

import (
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ServiceSummary aggregates one service's footprint over a query window.
type ServiceSummary struct {
	Service        string
	Samples        int
	AvgRSSMB       float64
	PeakRSSMB      float64
	AvgVRAMMB      float64
	PeakVRAMMB     float64
	AvgCPUPercent  float64
	PeakCPUPercent float64
	// IdlePercent is the fraction (0-100) of samples that were idle. Zero when no
	// sample carried an idle_seconds value.
	IdlePercent float64
	// LongestIdle is the longest contiguous idle stretch observed, derived from
	// consecutive idle samples spaced by the sampling interval. Zero when idle
	// data is absent.
	LongestIdle time.Duration
	// FirstSeen / LastSeen bound the window actually covered by this service's
	// rows (useful when the CSVs span more than the requested --since).
	FirstSeen time.Time
	LastSeen  time.Time
}

// QueryOptions filters the aggregation.
type QueryOptions struct {
	// Service, when non-empty, restricts the summary to that single service.
	Service string
	// Since, when non-zero, drops samples older than Since before now.
	Since time.Duration
	// IdleOnly restricts the aggregation to samples marked idle (idle_seconds>0).
	IdleOnly bool
	// Now overrides the reference time for Since filtering (testing seam).
	Now time.Time
}

// row is a parsed CSV record used only during aggregation.
type row struct {
	ts       time.Time
	service  string
	cpu      *float64
	rss      *float64
	vram     *int
	idleSecs *int
}

// isIdle reports whether the row counts as idle: an idle_seconds value that
// parsed and is strictly positive. A missing idle_seconds is NOT idle (the
// signal is simply unavailable in this branch).
func (r row) isIdle() bool {
	return r.idleSecs != nil && *r.idleSecs > 0
}

// Summarize reads every footprints CSV under dir, applies opts, and returns one
// ServiceSummary per service (sorted by service name). interval is the sampling
// cadence, used to bound the longest-idle-stretch computation. A missing dir
// yields an empty result, not an error.
func Summarize(dir string, opts QueryOptions, interval time.Duration) ([]ServiceSummary, error) {
	rows, err := readRows(dir)
	if err != nil {
		return nil, err
	}
	return summarizeRows(rows, opts, interval), nil
}

// readRows loads and parses all footprints-*.csv rows under dir.
func readRows(dir string) ([]row, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []row
	for _, e := range entries {
		if e.IsDir() || !dailyFilePattern.MatchString(e.Name()) {
			continue
		}
		rows, err := readFileRows(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // Skip a corrupt/partial file rather than failing the query.
		}
		out = append(out, rows...)
	}
	return out, nil
}

// readFileRows parses one CSV file into rows, keyed by the header order.
func readFileRows(path string) ([]row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // tolerate short/long lines from a partial write

	var rows []row
	first := true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // Stop at the first malformed record; keep what we parsed.
		}
		if first {
			first = false
			if len(rec) > 0 && rec[0] == "ts" {
				continue // header
			}
		}
		if len(rec) < len(csvHeader) {
			continue
		}
		parsed, ok := parseRecord(rec)
		if ok {
			rows = append(rows, parsed)
		}
	}
	return rows, nil
}

// parseRecord maps a CSV record (csvHeader order) into a row.
func parseRecord(rec []string) (row, bool) {
	ts, err := time.Parse(time.RFC3339, rec[0])
	if err != nil {
		return row{}, false
	}
	r := row{ts: ts, service: rec[2]}
	if v, err := strconv.ParseFloat(strings.TrimSpace(rec[4]), 64); err == nil && rec[4] != "" {
		r.cpu = &v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(rec[5]), 64); err == nil && rec[5] != "" {
		r.rss = &v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(rec[6])); err == nil && rec[6] != "" {
		r.vram = &v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(rec[8])); err == nil && rec[8] != "" {
		r.idleSecs = &v
	}
	return r, true
}

// summarizeRows is the pure aggregation core (testable with synthetic rows).
func summarizeRows(rows []row, opts QueryOptions, interval time.Duration) []ServiceSummary {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	if interval <= 0 {
		interval = DefaultInterval
	}

	// Group filtered rows by service.
	grouped := map[string][]row{}
	for _, r := range rows {
		if opts.Service != "" && r.service != opts.Service {
			continue
		}
		if opts.Since > 0 && r.ts.Before(now.Add(-opts.Since)) {
			continue
		}
		if opts.IdleOnly && !r.isIdle() {
			continue
		}
		grouped[r.service] = append(grouped[r.service], r)
	}

	summaries := make([]ServiceSummary, 0, len(grouped))
	for svc, srows := range grouped {
		summaries = append(summaries, aggregateService(svc, srows, interval))
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Service < summaries[j].Service
	})
	return summaries
}

// aggregateService computes one service's summary from its rows.
func aggregateService(service string, rows []row, interval time.Duration) ServiceSummary {
	sort.Slice(rows, func(i, j int) bool { return rows[i].ts.Before(rows[j].ts) })

	s := ServiceSummary{Service: service, Samples: len(rows)}
	if len(rows) == 0 {
		return s
	}
	s.FirstSeen = rows[0].ts
	s.LastSeen = rows[len(rows)-1].ts

	var rssSum, cpuSum, vramSum float64
	var rssN, cpuN, vramN int
	idleCount := 0

	// Longest idle stretch: count consecutive idle samples and multiply the
	// longest run of k idle samples by the sampling interval, so k idle samples
	// report k*interval of idle time. We cannot see between samples, so each idle
	// sample is credited one full interval; a lone idle sample is thus one
	// interval of observed idle time.
	var curRun int
	var longest int
	for _, r := range rows {
		if r.rss != nil {
			rssSum += *r.rss
			rssN++
			if *r.rss > s.PeakRSSMB {
				s.PeakRSSMB = *r.rss
			}
		}
		if r.cpu != nil {
			cpuSum += *r.cpu
			cpuN++
			if *r.cpu > s.PeakCPUPercent {
				s.PeakCPUPercent = *r.cpu
			}
		}
		if r.vram != nil {
			vf := float64(*r.vram)
			vramSum += vf
			vramN++
			if vf > s.PeakVRAMMB {
				s.PeakVRAMMB = vf
			}
		}
		if r.isIdle() {
			idleCount++
			curRun++
			if curRun > longest {
				longest = curRun
			}
		} else {
			curRun = 0
		}
	}

	if rssN > 0 {
		s.AvgRSSMB = rssSum / float64(rssN)
	}
	if cpuN > 0 {
		s.AvgCPUPercent = cpuSum / float64(cpuN)
	}
	if vramN > 0 {
		s.AvgVRAMMB = vramSum / float64(vramN)
	}
	if len(rows) > 0 {
		s.IdlePercent = float64(idleCount) / float64(len(rows)) * 100
	}
	if longest > 0 {
		s.LongestIdle = time.Duration(longest) * interval
	}
	return s
}
