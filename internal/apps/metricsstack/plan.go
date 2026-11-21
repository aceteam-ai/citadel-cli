// Package metricsstack renders an inactive operator-managed S2 candidate. It
// neither registers an app nor installs, starts, probes or authorizes anything.
package metricsstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
)

type Phase string

const (
	Stock  Phase = "stock"
	Native Phase = "native"
)

type Sizing struct {
	Phase                                                       Phase
	ActiveSeries, ScrapeIntervalSeconds, MeasuredBytesPerSample uint64
	HeadWALBytes, OtherVolumeBytes, ReserveFreeBytes            uint64
	RetentionSizeBytes, DiskBudgetBytes, MarginPercent          uint64
}

// ProjectedBytes mirrors S7's 30-day calculation, including overhead in the
// margin, and refuses overflow. It validates supplied measurements; it does
// not attest their provenance or replace the operator's enforced disk quota.
func (s Sizing) ProjectedBytes() (uint64, error) {
	if (s.Phase != Stock && s.Phase != Native) || s.ActiveSeries == 0 || s.ActiveSeries > 1000000 || s.ScrapeIntervalSeconds == 0 || s.ScrapeIntervalSeconds > 86400 || s.MeasuredBytesPerSample == 0 || s.RetentionSizeBytes == 0 || s.DiskBudgetBytes == 0 || s.ReserveFreeBytes == 0 || s.OtherVolumeBytes == 0 || s.MarginPercent > 10000 || (s.Phase == Stock && s.MarginPercent < 50) {
		return 0, errors.New("invalid metrics sizing")
	}
	samples := (uint64(30*86400) + s.ScrapeIntervalSeconds - 1) / s.ScrapeIntervalSeconds
	n := new(big.Int).SetUint64(s.ActiveSeries)
	n.Mul(n, new(big.Int).SetUint64(samples))
	n.Mul(n, new(big.Int).SetUint64(s.MeasuredBytesPerSample))
	n.Add(n, new(big.Int).SetUint64(s.HeadWALBytes))
	n.Add(n, new(big.Int).SetUint64(s.OtherVolumeBytes))
	n.Mul(n, new(big.Int).SetUint64(100+s.MarginPercent))
	n.Add(n, big.NewInt(99))
	n.Div(n, big.NewInt(100))
	if !n.IsUint64() || n.Uint64() > s.RetentionSizeBytes {
		return 0, errors.New("metrics retention budget insufficient")
	}
	bound := new(big.Int).SetUint64(s.RetentionSizeBytes)
	bound.Add(bound, new(big.Int).SetUint64(s.OtherVolumeBytes))
	bound.Add(bound, new(big.Int).SetUint64(s.ReserveFreeBytes))
	if bound.Cmp(new(big.Int).SetUint64(s.DiskBudgetBytes)) > 0 {
		return 0, errors.New("metrics disk budget insufficient")
	}
	return n.Uint64(), nil
}

type Resources struct {
	MemoryBytes, CPUMilli uint64
	PIDs, UID, GID        uint32
}

func (r Resources) valid() bool {
	return r.MemoryBytes >= 32<<20 && r.MemoryBytes <= 64<<30 && r.CPUMilli > 0 && r.CPUMilli <= 8000 && r.PIDs >= 1 && r.PIDs <= 1024 && r.UID > 0 && r.UID < 65536 && r.GID > 0 && r.GID < 65536
}

type Config struct {
	Instance string
	// Pins have no defaults. Registry/digest syntax is not a provenance proof.
	PrometheusImage, GrafanaImage string
	Prometheus, Grafana           Resources
	Sizing                        Sizing
}

var instancePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)
var imagePattern = regexp.MustCompile(`^ghcr\.io/aceteam-ai/[a-z0-9]+(?:[._-][a-z0-9]+)*@sha256:[a-f0-9]{64}$`)

type Candidate struct {
	// Baselines use empty scrape targets; S3 supplies the reviewed transport.
	ConfigFiles         map[string][]byte
	ComposeJSON         []byte
	Projected30DayBytes uint64
	RolloutAuthorized   bool
	RequiredEvidence    []string
}
type volume struct {
	External bool   `json:"external"`
	Name     string `json:"name"`
}
type mount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}
type service struct {
	Image        string            `json:"image"`
	User         string            `json:"user"`
	Command      []string          `json:"command,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
	Volumes      []mount           `json:"volumes"`
	Networks     []string          `json:"networks"`
	ReadOnly     bool              `json:"read_only"`
	CapDrop      []string          `json:"cap_drop"`
	SecurityOpt  []string          `json:"security_opt"`
	MemLimit     uint64            `json:"mem_limit"`
	MemSwapLimit uint64            `json:"memswap_limit"`
	CPUs         string            `json:"cpus"`
	PIDsLimit    uint32            `json:"pids_limit"`
	Tmpfs        []string          `json:"tmpfs"`
	Restart      string            `json:"restart"`
	Logging      map[string]any    `json:"logging"`
}
type compose struct {
	Name     string             `json:"name"`
	Services map[string]service `json:"services"`
	Volumes  map[string]volume  `json:"volumes"`
	Networks map[string]struct {
		Internal bool `json:"internal"`
	} `json:"networks"`
}

// BuildCandidate only produces bytes. External data/config volumes must already
// exist, with approved capacity and ownership. No host port, host networking,
// bind mount, runtime credential, generated password or admin identity is added.
func BuildCandidate(c Config) (Candidate, error) {
	if !instancePattern.MatchString(c.Instance) || !imagePattern.MatchString(c.PrometheusImage) || !imagePattern.MatchString(c.GrafanaImage) || !c.Prometheus.valid() || !c.Grafana.valid() {
		return Candidate{}, errors.New("invalid metrics package configuration")
	}
	projected, err := c.Sizing.ProjectedBytes()
	if err != nil {
		return Candidate{}, err
	}
	// Two bounded local log stores also consume disk. Measurements must include
	// these and actual Grafana/configuration growth in OtherVolumeBytes.
	if c.Sizing.OtherVolumeBytes < 20<<20 {
		return Candidate{}, errors.New("metrics log/storage overhead not budgeted")
	}
	makeService := func(image string, r Resources) service {
		return service{
			Image: image, User: fmt.Sprintf("%d:%d", r.UID, r.GID), Networks: []string{"metrics"}, ReadOnly: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			MemLimit: r.MemoryBytes, MemSwapLimit: r.MemoryBytes, CPUs: fmt.Sprintf("%d.%03d", r.CPUMilli/1000, r.CPUMilli%1000), PIDsLimit: r.PIDs,
			Tmpfs: []string{"/tmp:rw,noexec,nosuid,size=16777216"}, Restart: "no",
			Logging: map[string]any{"driver": "local", "options": map[string]string{"max-size": "5m", "max-file": "2"}},
		}
	}
	prom := makeService(c.PrometheusImage, c.Prometheus)
	prom.Command = []string{
		"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus",
		"--storage.tsdb.retention.time=30d", "--storage.tsdb.retention.size=" + strconv.FormatUint(c.Sizing.RetentionSizeBytes, 10) + "B",
		"--query.max-concurrency=2", "--query.timeout=10s", "--query.max-samples=500000",
	}
	prom.Volumes = []mount{{Type: "volume", Source: "prometheus_data", Target: "/prometheus"}, {Type: "volume", Source: "prometheus_config", Target: "/etc/prometheus", ReadOnly: true}}
	graf := makeService(c.GrafanaImage, c.Grafana)
	graf.Volumes = []mount{{Type: "volume", Source: "grafana_data", Target: "/var/lib/grafana"}, {Type: "volume", Source: "grafana_provisioning", Target: "/etc/grafana/provisioning", ReadOnly: true}}
	graf.Environment = map[string]string{
		"GF_AUTH_ANONYMOUS_ENABLED": "false", "GF_AUTH_BASIC_ENABLED": "false", "GF_AUTH_DISABLE_LOGIN_FORM": "true",
		"GF_SECURITY_DISABLE_INITIAL_ADMIN_CREATION": "true", "GF_USERS_ALLOW_SIGN_UP": "false", "GF_USERS_ALLOW_ORG_CREATE": "false",
		"GF_SNAPSHOTS_ENABLED": "false", "GF_SNAPSHOTS_EXTERNAL_ENABLED": "false", "GF_SNAPSHOTS_PUBLIC_MODE": "false", "GF_PUBLIC_DASHBOARDS_ENABLED": "false",
		"GF_UNIFIED_ALERTING_ENABLED": "false", "GF_ANALYTICS_REPORTING_ENABLED": "false", "GF_ANALYTICS_CHECK_FOR_UPDATES": "false", "GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES": "false",
	}
	data := compose{Name: "citadel-metrics-" + c.Instance, Services: map[string]service{"prometheus": prom, "grafana": graf}, Volumes: map[string]volume{}, Networks: map[string]struct {
		Internal bool `json:"internal"`
	}{"metrics": {Internal: true}}}
	for _, key := range []string{"prometheus_data", "prometheus_config", "grafana_data", "grafana_provisioning"} {
		data.Volumes[key] = volume{true, data.Name + "-" + key}
	}
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return Candidate{}, errors.New("metrics package rendering failed")
	}
	promConfig, _ := json.MarshalIndent(map[string]any{"global": map[string]string{"scrape_interval": strconv.FormatUint(c.Sizing.ScrapeIntervalSeconds, 10) + "s"}, "scrape_configs": []any{}}, "", "  ")
	dataSource, _ := json.MarshalIndent(map[string]any{"apiVersion": 1, "datasources": []any{map[string]any{"name": "Fleet metrics", "type": "prometheus", "access": "proxy", "url": "http://prometheus:9090", "isDefault": true, "editable": false}}}, "", "  ")
	return Candidate{ConfigFiles: map[string][]byte{"prometheus/prometheus.yml": promConfig, "grafana/datasources/metrics.yml": dataSource}, ComposeJSON: body, Projected30DayBytes: projected, RolloutAuthorized: false, RequiredEvidence: []string{
		"approved mirrored-image provenance and architecture compatibility",
		"operator-approved enforced storage capacity and non-root volume ownership",
		"Phase A cardinality, CPU/memory, disk peak and cold restore measurements",
		"reviewed read-only scrape/provisioning configuration with no secret-bearing labels",
		"S3 scoped scrape and current-org admin ingress after #11426 for native mode",
		"explicit operator installation approval; this candidate is not deployable by the catalog",
	}}, nil
}
