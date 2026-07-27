package footprint

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// csvHeader is the fixed column order for footprint CSVs. It is written once as
// the first line of each daily file. DuckDB / pandas read these headers directly
// (e.g. `duckdb -c "SELECT service, avg(rss_mb) FROM 'footprints/*.csv' GROUP BY 1"`).
// The first coreColumns are the original, pre-energy schema. The energy columns
// (power_w, energy_wh, power_source) are appended AFTER them so a reader that
// knows only the core schema still parses old and new files positionally.
var csvHeader = []string{
	"ts", "node_id", "service", "running",
	"cpu_pct", "rss_mb", "vram_mb", "gpu_util_pct", "idle_seconds",
	"power_w", "energy_wh", "power_source",
}

// coreColumns is the count of pre-energy columns. A row with at least this many
// fields is parseable; the energy columns are optional so footprint CSVs written
// by an older node (9 columns) remain readable after this schema grew.
const coreColumns = 9

// dailyFilePattern matches footprints-YYYY-MM-DD.csv so retention pruning only
// ever touches this package's own rotated files.
var dailyFilePattern = regexp.MustCompile(`^footprints-\d{4}-\d{2}-\d{2}\.csv$`)

// Store appends footprint samples to a per-day CSV under dir. It is safe for a
// single sampler goroutine; it does not guard against concurrent writers (there
// is only ever one sampler per node).
type Store struct {
	dir string
	// energy controls whether the three energy columns are written. When false
	// (energy sampling off) the store writes the original core schema only, so a
	// node with energy disabled produces byte-identical CSVs to the pre-energy
	// build.
	energy bool
}

// NewStore returns a Store writing to dir, creating the directory if needed. When
// energy is false the store omits the power_w / energy_wh / power_source columns
// entirely (header and rows).
func NewStore(dir string, energy bool) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("footprint: empty store dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("footprint: create store dir: %w", err)
	}
	return &Store{dir: dir, energy: energy}, nil
}

// header returns the CSV header for this store: the core (pre-energy) columns,
// plus the energy columns when energy sampling is enabled.
func (s *Store) header() []string {
	if s.energy {
		return csvHeader
	}
	return csvHeader[:coreColumns]
}

// Dir returns the store's directory (the footprints/ path).
func (s *Store) Dir() string { return s.dir }

// dailyFileName returns the file name for the given day.
func dailyFileName(day time.Time) string {
	return fmt.Sprintf("footprints-%s.csv", day.UTC().Format("2006-01-02"))
}

// filePathFor returns the absolute path of the daily file for ts.
func (s *Store) filePathFor(ts time.Time) string {
	return filepath.Join(s.dir, dailyFileName(ts))
}

// Append writes a batch of samples to the daily file for the first sample's
// timestamp. It writes the CSV header exactly once per file (on creation), so
// files rotate cleanly at UTC-day boundaries: a new day means a new file with a
// fresh header. An empty batch is a no-op.
func (s *Store) Append(samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	path := s.filePathFor(samples[0].Timestamp)

	// A file needs a header iff it does not yet exist (or is empty). Stat before
	// opening in append mode so we can decide whether to emit the header row.
	needHeader := false
	if info, err := os.Stat(path); os.IsNotExist(err) || (err == nil && info.Size() == 0) {
		needHeader = true
	} else if err != nil {
		return fmt.Errorf("footprint: stat %s: %w", path, err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("footprint: open %s: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if needHeader {
		if err := w.Write(s.header()); err != nil {
			return fmt.Errorf("footprint: write header: %w", err)
		}
	}
	for _, sm := range samples {
		if err := w.Write(sm.toRecord(s.energy)); err != nil {
			return fmt.Errorf("footprint: write row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("footprint: flush %s: %w", path, err)
	}
	return nil
}

// toRecord renders a sample as a CSV record in csvHeader column order. Unset
// optional metrics render as the empty string, so DuckDB reads them as NULL and
// a human greps them as blank, distinct from a measured zero. The three energy
// columns are appended only when energy is true; when false the record is the
// original core schema.
func (sm Sample) toRecord(energy bool) []string {
	rec := []string{
		sm.Timestamp.UTC().Format(time.RFC3339),
		sm.NodeID,
		sm.Service,
		strconv.FormatBool(sm.Running),
		floatField(sm.CPUPercent),
		floatField(sm.RSSMB),
		intField(sm.VRAMMB),
		floatField(sm.GPUUtilPercent),
		intField(sm.IdleSeconds),
	}
	if energy {
		rec = append(rec,
			floatField(sm.PowerW),
			floatField(sm.EnergyWh),
			string(sm.PowerSource),
		)
	}
	return rec
}

func floatField(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func intField(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

// Prune deletes daily footprint files whose day is older than retentionDays
// relative to now, so the log never becomes an unbounded disk hog. Only files
// matching the footprints-YYYY-MM-DD.csv pattern are ever considered — unrelated
// files in the directory are left untouched. A retentionDays <= 0 is treated as
// "keep everything" (pruning disabled). Returns the names of pruned files.
func (s *Store) Prune(now time.Time, retentionDays int) ([]string, error) {
	if retentionDays <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("footprint: read store dir: %w", err)
	}
	// Cutoff is the oldest UTC day we keep. Files dated strictly before this are
	// pruned. now-retentionDays: with retentionDays=7, today plus the previous 6
	// days are kept (7 days of data).
	cutoff := now.UTC().AddDate(0, 0, -(retentionDays - 1)).Truncate(24 * time.Hour)

	var pruned []string
	for _, e := range entries {
		if e.IsDir() || !dailyFilePattern.MatchString(e.Name()) {
			continue
		}
		day, err := dayFromFileName(e.Name())
		if err != nil {
			continue // Malformed date despite pattern match: leave it alone.
		}
		if day.Before(cutoff) {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {
				return pruned, fmt.Errorf("footprint: remove %s: %w", e.Name(), err)
			}
			pruned = append(pruned, e.Name())
		}
	}
	return pruned, nil
}

// dayFromFileName parses the YYYY-MM-DD date out of a footprints-*.csv name.
func dayFromFileName(name string) (time.Time, error) {
	// footprints-2006-01-02.csv -> 2006-01-02
	base := name[len("footprints-") : len(name)-len(".csv")]
	return time.Parse("2006-01-02", base)
}
