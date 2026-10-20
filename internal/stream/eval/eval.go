// Package eval is the bounded offline driver for the shared stream rule engine.
// It opens only explicitly selected local input data. It does not start a live
// source, read a clock or credentials, or contact a service.
package eval

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/stream/event"
	"github.com/aceteam-ai/citadel-cli/internal/stream/input"
	"github.com/aceteam-ai/citadel-cli/internal/stream/rules"
)

const (
	MaxFiles      = 128
	MaxTotalBytes = 256 << 20
	MaxAlerts     = 100000
	// A stable replay stamp, not the date on which the evaluator is run. The
	// envelope timeline advances one millisecond per input record per source.
	ReplayEpochNS int64 = 1767225600000000000 // 2026-01-01T00:00:00Z
)

type Options struct {
	TimeMode        string
	AllowedLateness time.Duration
	ExpectedPath    string
}

type Scenario struct {
	ID          string            `json:"id"`
	SourceID    string            `json:"source_id"`
	InputDigest string            `json:"input_digest"`
	Bytes       uint64            `json:"bytes"`
	Events      uint64            `json:"events"`
	Matches     []rules.Alert     `json:"matches"`
	Stats       rules.EngineStats `json:"stats"`
}

type Report struct {
	SchemaVersion     string          `json:"schema_version"`
	PackID            string          `json:"pack_id"`
	PackVersion       string          `json:"pack_version"`
	PackDigest        string          `json:"pack_digest"`
	TimeMode          string          `json:"time_mode"`
	AllowedLatenessNS int64           `json:"allowed_lateness_ns,string"`
	ReplayEpochNS     int64           `json:"replay_epoch_ns,string"`
	ReplayStepNS      int64           `json:"replay_step_ns,string"`
	Refused           []rules.Refusal `json:"refused"`
	Scenarios         []Scenario      `json:"scenarios"`
	Comparison        *Comparison     `json:"comparison,omitempty"`
	OutputDigest      string          `json:"output_digest,omitempty"`
}

// Run loads a reviewed pack and evaluates one regular file, or the sorted
// direct *.log and *.jsonl children of one directory. Each file is a distinct
// scenario with fresh state; directory order never mixes independent histories.
func Run(ctx context.Context, packPath, logsPath string, options Options) (Report, error) {
	if options.TimeMode == "" {
		options.TimeMode = "ingest"
	}
	if options.TimeMode != "ingest" && options.TimeMode != "event" {
		return Report{}, errors.New("stream eval time mode must be ingest or event")
	}
	if options.AllowedLateness < 0 || options.AllowedLateness > time.Hour {
		return Report{}, errors.New("stream eval lateness must be between zero and one hour")
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	pack, err := rules.LoadPack(packPath)
	if err != nil {
		var refusal *rules.CompileError
		if errors.As(err, &refusal) && len(refusal.RuleID) <= 128 && len(refusal.Reason) <= 256 &&
			strings.IndexFunc(refusal.RuleID+refusal.Reason, func(char rune) bool { return char < 32 || char == 127 }) < 0 {
			return Report{}, fmt.Errorf("stream eval rule %s refused: %s", refusal.RuleID, refusal.Reason)
		}
		return Report{}, errors.New("stream eval could not load the rule pack")
	}
	files, root, err := discover(logsPath)
	if err != nil {
		return Report{}, err
	}
	defer root.Close()
	report := Report{
		SchemaVersion:     "stream.eval.v1",
		PackID:            pack.Manifest.ID,
		PackVersion:       pack.Manifest.Version,
		PackDigest:        pack.Digest,
		TimeMode:          options.TimeMode,
		AllowedLatenessNS: int64(options.AllowedLateness),
		ReplayEpochNS:     ReplayEpochNS,
		ReplayStepNS:      int64(time.Millisecond),
		Refused:           append([]rules.Refusal{}, pack.Refused...),
		Scenarios:         []Scenario{},
	}
	var totalBytes uint64
	totalAlerts := 0
	for index, name := range files {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		scenario, err := runScenario(ctx, root, name, index+1, pack, options, &totalBytes, &totalAlerts)
		if err != nil {
			return Report{}, err
		}
		report.Scenarios = append(report.Scenarios, scenario)
	}
	if options.ExpectedPath != "" {
		comparison, err := compareExpected(options.ExpectedPath, report.Scenarios)
		if err != nil {
			return Report{}, err
		}
		report.Comparison = comparison
	}
	canonical, err := json.Marshal(report)
	if err != nil {
		return Report{}, errors.New("stream eval could not encode the report")
	}
	digest := sha256.Sum256(canonical)
	report.OutputDigest = "sha256:" + hex.EncodeToString(digest[:])
	return report, nil
}

func discover(path string) ([]string, *os.Root, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("stream eval input is unavailable or a symlink")
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, nil, errors.New("stream eval input must be a regular file or directory")
	}
	parent := filepath.Dir(path)
	if info.IsDir() {
		parent = path
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, nil, errors.New("stream eval could not open the input directory")
	}
	if info.IsDir() {
		opened, statErr := root.Stat(".")
		if statErr != nil || !os.SameFile(info, opened) {
			root.Close()
			return nil, nil, errors.New("stream eval input directory changed while opening")
		}
	}
	var names []string
	if !info.IsDir() {
		names = []string{filepath.Base(path)}
	} else {
		directory, openErr := root.Open(".")
		if openErr != nil {
			root.Close()
			return nil, nil, errors.New("stream eval could not enumerate the input directory")
		}
		// Bound discovery itself, not merely the selected output page. Inspect
		// one entry at a time, and refuse directories with excessive entries.
		entriesSeen := 0
		for {
			entries, readErr := directory.ReadDir(1)
			if readErr != nil && readErr != io.EOF {
				directory.Close()
				root.Close()
				return nil, nil, errors.New("stream eval could not enumerate the input directory")
			}
			for _, entry := range entries {
				entriesSeen++
				if entriesSeen > 4096 {
					directory.Close()
					root.Close()
					return nil, nil, errors.New("stream eval directory exceeds the entry limit")
				}
				extension := filepath.Ext(entry.Name())
				if extension != ".log" && extension != ".jsonl" {
					continue
				}
				if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !entry.Type().IsRegular() {
					directory.Close()
					root.Close()
					return nil, nil, errors.New("stream eval selected input is not a regular file")
				}
				names = append(names, entry.Name())
				if len(names) > MaxFiles {
					directory.Close()
					root.Close()
					return nil, nil, errors.New("stream eval exceeds the input file limit")
				}
			}
			if readErr == io.EOF {
				break
			}
		}
		directory.Close()
	}
	if len(names) == 0 {
		root.Close()
		return nil, nil, errors.New("stream eval found no input files")
	}
	sort.Strings(names)
	ids := make(map[string]bool)
	for _, name := range names {
		id := strings.TrimSuffix(name, filepath.Ext(name))
		if !validSourceName(name) || id == "" || ids[id] {
			root.Close()
			return nil, nil, errors.New("stream eval input has an invalid or duplicate scenario name")
		}
		ids[id] = true
	}
	return names, root, nil
}

func validSourceName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._-", char)) {
			return false
		}
	}
	return true
}

func runScenario(ctx context.Context, root *os.Root, name string, sourceIndex int, pack *rules.Pack, options Options, totalBytes *uint64, totalAlerts *int) (Scenario, error) {
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() {
		return Scenario{}, errors.New("stream eval input changed or is not a regular file")
	}
	if before.Size() < 0 || uint64(before.Size()) > MaxTotalBytes-*totalBytes {
		return Scenario{}, errors.New("stream eval exceeds the total byte limit")
	}
	file, err := input.OpenRegular(root, name)
	if err != nil {
		return Scenario{}, errors.New("stream eval could not open an input file")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return Scenario{}, errors.New("stream eval input changed while opening")
	}
	engine, err := rules.NewEngine(pack, rules.Options{
		TimeMode:        options.TimeMode,
		AllowedLateness: options.AllowedLateness,
	})
	if err != nil {
		return Scenario{}, errors.New("stream eval could not initialize the rule engine")
	}
	scenario := Scenario{ID: strings.TrimSuffix(name, filepath.Ext(name)), SourceID: name, Matches: []rules.Alert{}}
	inputHash := sha256.New()
	reader := bufio.NewReaderSize(io.LimitReader(file, int64(MaxTotalBytes-*totalBytes)+1), event.DefaultMaxLineBytes+1)
	parserOptions := pack.ParserOptions()
	parserOptions.MaxLineBytes = event.DefaultMaxLineBytes
	var offset uint64
	for {
		if err := ctx.Err(); err != nil {
			return Scenario{}, err
		}
		line, readErr := reader.ReadSlice('\n')
		if readErr != nil && readErr != io.EOF {
			if readErr == bufio.ErrBufferFull {
				return Scenario{}, errors.New("stream eval input exceeds the line byte limit")
			}
			return Scenario{}, errors.New("stream eval could not read an input file")
		}
		if len(line) > 0 {
			if len(line) > event.DefaultMaxLineBytes {
				return Scenario{}, errors.New("stream eval input exceeds the line byte limit")
			}
			if uint64(len(line)) > MaxTotalBytes-*totalBytes {
				return Scenario{}, errors.New("stream eval exceeds the total byte limit")
			}
			*totalBytes += uint64(len(line))
			scenario.Bytes += uint64(len(line))
			scenario.Events++
			_, _ = inputHash.Write(line)
			stamp := int64(scenario.Events) * int64(time.Millisecond)
			meta := event.Envelope{
				Seq:        scenario.Events,
				TMonoNS:    stamp,
				TWallNS:    ReplayEpochNS + stamp,
				ClockState: "offline_replay",
				SourceID:   name,
				NodeID:     "offline-eval",
				RawRef:     name + ":" + strconv.FormatUint(offset, 10),
			}
			parsed, err := event.Parse(pack.Manifest.Parser, string(line), meta, parserOptions)
			if err != nil {
				return Scenario{}, fmt.Errorf("stream eval could not parse source %d record %d", sourceIndex, scenario.Events)
			}
			alerts, err := engine.Evaluate(parsed)
			if err != nil {
				return Scenario{}, fmt.Errorf("stream eval could not evaluate source %d record %d", sourceIndex, scenario.Events)
			}
			if err := appendAlerts(&scenario, alerts, totalAlerts); err != nil {
				return Scenario{}, err
			}
			offset += uint64(len(line))
		}
		if readErr == io.EOF {
			break
		}
	}
	alerts, err := engine.Flush()
	if err != nil {
		return Scenario{}, errors.New("stream eval could not flush the rule engine")
	}
	if err := appendAlerts(&scenario, alerts, totalAlerts); err != nil {
		return Scenario{}, err
	}
	rules.SortAlerts(scenario.Matches)
	scenario.Stats = engine.Stats()
	scenario.InputDigest = "sha256:" + hex.EncodeToString(inputHash.Sum(nil))
	return scenario, nil
}

func appendAlerts(scenario *Scenario, alerts []rules.Alert, total *int) error {
	if *total < 0 || *total > MaxAlerts || len(alerts) > MaxAlerts-*total {
		return errors.New("stream eval exceeds the alert limit")
	}
	*total += len(alerts)
	scenario.Matches = append(scenario.Matches, alerts...)
	return nil
}
