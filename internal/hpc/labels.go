package hpc

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Slurm environment variables read for enrollment labels.
const (
	EnvClusterName = "SLURM_CLUSTER_NAME"
	EnvPartition   = "SLURM_JOB_PARTITION"
	EnvAccount     = "SLURM_JOB_ACCOUNT"
	EnvNodeName    = "SLURMD_NODENAME"
	EnvJobEndTime  = "SLURM_JOB_END_TIME"
	EnvVisibleGPUs = "CUDA_VISIBLE_DEVICES"
)

// labelPattern mirrors the platform's enrollment label rule. Validating here
// gives a readable local error instead of a bare 400 after the token burns.
var labelPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Enrollment wall-time bounds, matching the platform's redemption checks.
const (
	MinWallRemaining = 60 * time.Second
	MaxWallRemaining = 7 * 24 * time.Hour
	maxAllocatedGPUs = 256
)

// Labels are the allocation labels sent with an enrollment redemption. The
// platform rejects unknown keys, so this is the complete set.
type Labels struct {
	Cluster       string
	Partition     string
	Account       string
	SlurmJobID    string
	Host          string
	GPUType       string // optional
	AllocatedGPUs int
	WallTimeEnd   time.Time
}

// Map returns the wire form. gpu_type is omitted when unknown.
func (l Labels) Map() map[string]any {
	m := map[string]any{
		"cluster":        l.Cluster,
		"partition":      l.Partition,
		"account":        l.Account,
		"slurm_job_id":   l.SlurmJobID,
		"host":           l.Host,
		"allocated_gpus": l.AllocatedGPUs,
		"wall_time_end":  l.WallTimeEnd.UTC().Format(time.RFC3339),
	}
	if l.GPUType != "" {
		m["gpu_type"] = l.GPUType
	}
	return m
}

// LabelInputs are the already-read values labels are derived from.
type LabelInputs struct {
	Getenv   func(string) string
	Hostname string
	// GPUNames returns the model names of the visible devices (nil when no
	// probe is available). It is only consulted when at least one GPU is
	// allocated.
	GPUNames func(visible []string) []string
	// WallTimeEnd overrides SLURM_JOB_END_TIME (RFC 3339), for an allocation
	// whose environment does not carry the end time.
	WallTimeEnd string
	Now         time.Time
}

// VisibleGPUs returns the CUDA_VISIBLE_DEVICES entries: the GPUs Slurm
// allocated to this job. Unset or empty means none.
func VisibleGPUs(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" || part == "-1" || strings.EqualFold(part, "NoDevFiles") {
			continue
		}
		out = append(out, part)
	}
	return out
}

// SanitizeLabel maps a free-form value (for example a GPU model name with
// spaces) onto the label alphabet. It returns "" if nothing usable remains.
func SanitizeLabel(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.TrimSpace(s) {
		ok := r == '.' || r == '_' || r == ':' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			dash = r == '-'
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 128 {
		out = strings.TrimRight(out[:128], "-")
	}
	return out
}

// ResolveLabels builds and validates the enrollment labels. A required value
// that is missing or malformed is an error that names the variable; a job
// with no end time is refused rather than given an invented one.
func ResolveLabels(in LabelInputs) (Labels, error) {
	get := in.Getenv
	if get == nil {
		get = func(string) string { return "" }
	}
	var problems []string
	field := func(name, value string) string {
		value = strings.TrimSpace(value)
		if value == "" {
			problems = append(problems, name+" is not set")
		} else if !labelPattern.MatchString(value) {
			problems = append(problems, name+" has characters the platform does not accept")
		}
		return value
	}
	l := Labels{
		Cluster:    field(EnvClusterName, get(EnvClusterName)),
		Partition:  field(EnvPartition, get(EnvPartition)),
		Account:    field(EnvAccount, get(EnvAccount)),
		SlurmJobID: field(EnvJobID, get(EnvJobID)),
	}
	host := strings.TrimSpace(get(EnvNodeName))
	if host == "" {
		host = SanitizeLabel(in.Hostname)
	}
	l.Host = field(EnvNodeName+" (or hostname)", host)

	visible := VisibleGPUs(get(EnvVisibleGPUs))
	l.AllocatedGPUs = len(visible)
	if l.AllocatedGPUs > maxAllocatedGPUs {
		problems = append(problems, fmt.Sprintf("%s lists more than %d devices", EnvVisibleGPUs, maxAllocatedGPUs))
	}
	if l.AllocatedGPUs > 0 && in.GPUNames != nil {
		if names := in.GPUNames(visible); len(names) > 0 {
			l.GPUType = SanitizeLabel(names[0])
		}
	}

	end, err := wallTimeEnd(get(EnvJobEndTime), in.WallTimeEnd)
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		remaining := end.Sub(in.Now)
		switch {
		case remaining <= MinWallRemaining:
			problems = append(problems, "the allocation ends in less than a minute")
		case remaining > MaxWallRemaining:
			problems = append(problems, "the allocation wall time ends more than 7 days ahead")
		}
		l.WallTimeEnd = end
	}
	if len(problems) > 0 {
		return Labels{}, errors.New("hpc enrollment labels: " + strings.Join(problems, "; "))
	}
	return l, nil
}

func wallTimeEnd(envValue, override string) (time.Time, error) {
	if override = strings.TrimSpace(override); override != "" {
		t, err := time.Parse(time.RFC3339, override)
		if err != nil {
			return time.Time{}, fmt.Errorf("wall time end %q is not RFC 3339", override)
		}
		return t, nil
	}
	envValue = strings.TrimSpace(envValue)
	if envValue == "" {
		return time.Time{}, fmt.Errorf("%s is not set (a job with no time limit cannot enroll; pass --wall-time-end)", EnvJobEndTime)
	}
	secs, err := strconv.ParseInt(envValue, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, fmt.Errorf("%s %q is not a Unix time", EnvJobEndTime, envValue)
	}
	return time.Unix(secs, 0).UTC(), nil
}

// ParseGPUNames reads `nvidia-smi --query-gpu=index,uuid,name
// --format=csv,noheader` output and returns the names of the visible devices,
// matched by index or UUID, in CUDA_VISIBLE_DEVICES order.
func ParseGPUNames(csv string, visible []string) []string {
	byKey := map[string]string{}
	for _, line := range strings.Split(csv, "\n") {
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			continue
		}
		name := strings.TrimSpace(parts[2])
		byKey[strings.TrimSpace(parts[0])] = name
		byKey[strings.TrimSpace(parts[1])] = name
	}
	var out []string
	for _, v := range visible {
		if name, ok := byKey[v]; ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}
