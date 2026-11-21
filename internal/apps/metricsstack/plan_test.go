package metricsstack

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func fixture() Config {
	return Config{Instance: "fixture", PrometheusImage: "ghcr.io/aceteam-ai/prometheus@sha256:" + strings.Repeat("a", 64), GrafanaImage: "ghcr.io/aceteam-ai/grafana@sha256:" + strings.Repeat("b", 64), Prometheus: Resources{MemoryBytes: 512 << 20, CPUMilli: 250, PIDs: 64, UID: 10001, GID: 10001}, Grafana: Resources{MemoryBytes: 256 << 20, CPUMilli: 200, PIDs: 64, UID: 10001, GID: 10001}, Sizing: Sizing{Phase: Stock, ActiveSeries: 120, ScrapeIntervalSeconds: 30, MeasuredBytesPerSample: 2, HeadWALBytes: 512 << 20, OtherVolumeBytes: 128 << 20, ReserveFreeBytes: 1 << 30, RetentionSizeBytes: 2 << 30, DiskBudgetBytes: 4 << 30, MarginPercent: 50}}
}
func TestCandidateIsInertPrivatePersistentAndBounded(t *testing.T) {
	cfg := fixture()
	candidate, e := BuildCandidate(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if candidate.RolloutAuthorized || len(candidate.RequiredEvidence) < 5 {
		t.Fatal("candidate granted rollout authority")
	}
	var plan compose
	if e = json.Unmarshal(candidate.ComposeJSON, &plan); e != nil {
		t.Fatal(e)
	}
	if len(plan.Services) != 2 || len(plan.Volumes) != 4 || len(plan.Networks) != 1 || !plan.Networks["metrics"].Internal {
		t.Fatal("wrong package isolation")
	}
	for name, s := range plan.Services {
		if !s.ReadOnly || s.User != "10001:10001" || s.MemLimit == 0 || s.MemSwapLimit != s.MemLimit || s.PIDsLimit != 64 || s.Restart != "no" || len(s.CapDrop) != 1 || s.CapDrop[0] != "ALL" || len(s.SecurityOpt) != 1 || s.SecurityOpt[0] != "no-new-privileges:true" {
			t.Fatalf("unsafe %s settings: %+v", name, s)
		}
		if len(s.Networks) != 1 || s.Networks[0] != "metrics" || len(s.Tmpfs) != 1 || !strings.Contains(s.Tmpfs[0], "size=16777216") {
			t.Fatal("unbounded scratch/network")
		}
		if len(s.Volumes) != 2 || s.Volumes[0].ReadOnly || !s.Volumes[1].ReadOnly || s.Volumes[0].Type != "volume" {
			t.Fatal("persistence or configuration write boundary")
		}
	}
	for _, v := range plan.Volumes {
		if !v.External || !strings.HasPrefix(v.Name, "citadel-metrics-fixture-") {
			t.Fatal("implicit volume allocation or wrong ownership namespace")
		}
	}
	raw := string(candidate.ComposeJSON)
	for _, forbidden := range []string{`"ports"`, `"network_mode"`, `"privileged"`, `"devices"`, `docker.sock`, `/proc`, `/sys`, `/home`, `admin_password`, `oauth_client_secret`, `enable-admin-api`, `enable-lifecycle`, `remote-write`, `alertmanager`} {
		if strings.Contains(raw, forbidden) {
			t.Fatal("unexpected authority or secret surface", forbidden)
		}
	}
	if plan.Services["prometheus"].CPUs != "0.250" || plan.Services["grafana"].CPUs != "0.200" {
		t.Fatal("CPU cap conversion")
	}
	commands := strings.Join(plan.Services["prometheus"].Command, " ")
	if !strings.Contains(commands, "--storage.tsdb.retention.time=30d") || !strings.Contains(commands, "--storage.tsdb.retention.size=2147483648B") || !strings.Contains(commands, "--query.max-concurrency=2") || !strings.Contains(commands, "--query.timeout=10s") {
		t.Fatal("retention/query budget missing", commands)
	}
	for _, setting := range []string{"GF_AUTH_ANONYMOUS_ENABLED", "GF_AUTH_BASIC_ENABLED", "GF_USERS_ALLOW_SIGN_UP", "GF_USERS_ALLOW_ORG_CREATE", "GF_SNAPSHOTS_ENABLED", "GF_SNAPSHOTS_EXTERNAL_ENABLED", "GF_SNAPSHOTS_PUBLIC_MODE", "GF_PUBLIC_DASHBOARDS_ENABLED", "GF_UNIFIED_ALERTING_ENABLED"} {
		if plan.Services["grafana"].Environment[setting] != "false" {
			t.Fatal("unsafe Grafana setting", setting)
		}
	}
	if plan.Services["grafana"].Environment["GF_SECURITY_DISABLE_INITIAL_ADMIN_CREATION"] != "true" {
		t.Fatal("bootstrap admin created")
	}
}
func TestNoImageDefaultsOrRegistryWidening(t *testing.T) {
	for _, image := range []string{"", "prom/prometheus:latest", "docker.io/grafana/grafana:latest", "ghcr.io/aceteam-ai/prometheus:latest", "ghcr.io/foreign/prometheus@sha256:" + strings.Repeat("a", 64), "ghcr.io/aceteam-ai/../prometheus@sha256:" + strings.Repeat("a", 64), "ghcr.io/aceteam-ai/prometheus@sha256:" + strings.Repeat("A", 64)} {
		t.Run(image, func(t *testing.T) {
			c := fixture()
			c.PrometheusImage = image
			if _, e := BuildCandidate(c); e == nil {
				t.Fatal("accepted unpinned or unapproved namespace")
			}
		})
	}
}
func TestPackageRefusesUnsafeIdentifiersAndResources(t *testing.T) {
	for _, id := range []string{"", "../x", "fixture-second-network\n", "fixture/../../host", "a" + strings.Repeat("b", 32)} {
		c := fixture()
		c.Instance = id
		if _, e := BuildCandidate(c); e == nil {
			t.Fatal("accepted unsafe namespace")
		}
	}
	for _, mutate := range []func(*Resources){func(r *Resources) { r.UID = 0 }, func(r *Resources) { r.GID = 0 }, func(r *Resources) { r.MemoryBytes = 0 }, func(r *Resources) { r.CPUMilli = 0 }, func(r *Resources) { r.CPUMilli = 8001 }, func(r *Resources) { r.PIDs = 0 }, func(r *Resources) { r.PIDs = 1025 }} {
		c := fixture()
		mutate(&c.Grafana)
		if _, e := BuildCandidate(c); e == nil {
			t.Fatal("accepted unsafe resources")
		}
	}
}
func TestSizingMatchesS7IncludesOverheadAndRoundsUp(t *testing.T) {
	s := fixture().Sizing
	// ceil((120 * 86400 * 2 + 512MiB + 128MiB) * 1.5)
	want := uint64(1037736960)
	got, e := s.ProjectedBytes()
	if e != nil || got != want {
		t.Fatalf("got %d want %d err=%v", got, want, e)
	}
	s.Phase = Native
	s.ActiveSeries = 1
	s.ScrapeIntervalSeconds = 86400
	s.MeasuredBytesPerSample = 1
	s.HeadWALBytes = 0
	s.OtherVolumeBytes = 1
	s.MarginPercent = 1
	s.RetentionSizeBytes = 100
	s.ReserveFreeBytes = 1
	s.DiskBudgetBytes = 102
	if n, e := s.ProjectedBytes(); e != nil || n != 32 {
		t.Fatalf("native margin/ceil: %d %v", n, e)
	}
	s.Phase = Stock
	if _, e := s.ProjectedBytes(); e == nil {
		t.Fatal("Phase A 50 percent rule lost")
	}
}
func TestSizingRefusesInsufficientCapsAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Sizing)
	}{
		{"retention", func(s *Sizing) { s.RetentionSizeBytes = 1 }},
		{"volume plus reserve", func(s *Sizing) { s.DiskBudgetBytes = s.RetentionSizeBytes }},
		{"WAL", func(s *Sizing) { s.HeadWALBytes = math.MaxUint64 }},
		{"sample multiply", func(s *Sizing) { s.MeasuredBytesPerSample = math.MaxUint64 }},
		{"zero samples", func(s *Sizing) { s.ActiveSeries = 0 }},
		{"interval", func(s *Sizing) { s.ScrapeIntervalSeconds = 0 }},
		{"unbounded margin", func(s *Sizing) { s.MarginPercent = math.MaxUint64 }},
		{"Phase A margin", func(s *Sizing) { s.MarginPercent = 49 }},
		{"unknown phase", func(s *Sizing) { s.Phase = "production" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture().Sizing
			tc.change(&s)
			if _, e := s.ProjectedBytes(); e == nil {
				t.Fatal("accepted insufficient or overflowed capacity")
			}
		})
	}
	c := fixture()
	c.Sizing.OtherVolumeBytes = 1
	if _, e := BuildCandidate(c); e == nil {
		t.Fatal("omitted bounded container logs")
	}
}
func TestRestartCandidatePreservesExactVolumesAndPins(t *testing.T) {
	c := fixture()
	first, e := BuildCandidate(c)
	if e != nil {
		t.Fatal(e)
	}
	second, e := BuildCandidate(c)
	if e != nil {
		t.Fatal(e)
	}
	if string(first.ComposeJSON) != string(second.ComposeJSON) {
		t.Fatal("restart candidate changed persistent storage or image identity")
	}
}

func TestGeneratedConfigsContainNoTargetsCredentialsOrPager(t *testing.T) {
	candidate, e := BuildCandidate(fixture())
	if e != nil {
		t.Fatal(e)
	}
	if len(candidate.ConfigFiles) != 2 {
		t.Fatal("missing safe baseline configuration")
	}
	var prom map[string]any
	if e = json.Unmarshal(candidate.ConfigFiles["prometheus/prometheus.yml"], &prom); e != nil {
		t.Fatal(e)
	}
	if len(prom["scrape_configs"].([]any)) != 0 {
		t.Fatal("source candidate activated a scrape target")
	}
	if prom["global"].(map[string]any)["scrape_interval"] != "30s" {
		t.Fatal("sizing interval diverged from config")
	}
	for _, key := range []string{"alerting", "remote_write", "rule_files"} {
		if _, ok := prom[key]; ok {
			t.Fatal("unapproved pager or transport")
		}
	}
	var graf map[string]any
	if e = json.Unmarshal(candidate.ConfigFiles["grafana/datasources/metrics.yml"], &graf); e != nil {
		t.Fatal(e)
	}
	ds := graf["datasources"].([]any)[0].(map[string]any)
	if ds["url"] != "http://prometheus:9090" || ds["editable"] != false || ds["access"] != "proxy" {
		t.Fatal("unreviewed datasource route or edit authority")
	}
	for _, body := range candidate.ConfigFiles {
		for _, bad := range []string{"password", "token", "secret", "authorization", "http://public"} {
			if strings.Contains(string(body), bad) {
				t.Fatal("credential or public target in baseline", bad)
			}
		}
	}
}

func TestFreeReserveCannotBeSpentByRetention(t *testing.T) {
	s := fixture().Sizing
	s.DiskBudgetBytes = s.RetentionSizeBytes + s.OtherVolumeBytes + s.ReserveFreeBytes - 1
	if _, e := s.ProjectedBytes(); e == nil {
		t.Fatal("accepted retention that spends reserved free disk")
	}
}
