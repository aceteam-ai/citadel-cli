package desktopmodel

import (
	"bytes"
	"math"
	"reflect"
	"testing"
)

func validFacts(memory uint64) machineFacts {
	return machineFacts{OS: machineOSDarwin, Architecture: machineArchitectureARM64, TotalMemoryKnown: true, TotalMemoryBytes: memory}
}

func requirePlan(t *testing.T, facts machineFacts) automaticPlan {
	t.Helper()
	p, err := planAutomatic(facts)
	if err != nil {
		t.Fatalf("planAutomatic: %v", err)
	}
	if !p.valid() {
		t.Fatal("planner returned invalid plan")
	}
	return p
}

func TestPlanAutomaticExactMemoryBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		memory      uint64
		eligibility string
		reason      string
		model       string
		floor       uint64
	}{
		{"below 16 GiB", 16*gib - 1, eligibilityIneligible, reasonMemoryBelowMinimum, "", 0},
		{"exactly 16 GiB", 16 * gib, eligibilityEligible, "", "qwen3:4b", 8 * gib},
		{"below 32 GiB", 32*gib - 1, eligibilityEligible, "", "qwen3:4b", 8 * gib},
		{"exactly 32 GiB", 32 * gib, eligibilityEligible, "", "qwen3:8b", 14 * gib},
		{"maximum", math.MaxUint64, eligibilityEligible, "", "qwen3:8b", 14 * gib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := requirePlan(t, validFacts(tt.memory))
			if p.eligibility != tt.eligibility || p.reason != tt.reason || p.model.ID != tt.model || p.model.MinimumFreeDiskBytes != tt.floor {
				t.Fatalf("plan = %#v", p)
			}
		})
	}
}

func TestPlanAutomaticRejectsInvalidFactsBeforeClassification(t *testing.T) {
	tests := []machineFacts{
		{},
		{OS: machineOSUnknown, Architecture: machineArchitectureARM64, TotalMemoryKnown: true, TotalMemoryBytes: 16 * gib},
		{OS: machineOS(99), Architecture: machineArchitectureARM64, TotalMemoryKnown: true, TotalMemoryBytes: 16 * gib},
		{OS: machineOSDarwin, Architecture: machineArchitectureUnknown, TotalMemoryKnown: true, TotalMemoryBytes: 16 * gib},
		{OS: machineOSDarwin, Architecture: machineArchitecture(99), TotalMemoryKnown: true, TotalMemoryBytes: 16 * gib},
		{OS: machineOSDarwin, Architecture: machineArchitectureARM64, TotalMemoryKnown: false, TotalMemoryBytes: 0},
		{OS: machineOSDarwin, Architecture: machineArchitectureARM64, TotalMemoryKnown: false, TotalMemoryBytes: 16 * gib},
		{OS: machineOSDarwin, Architecture: machineArchitectureARM64, TotalMemoryKnown: true, TotalMemoryBytes: 0},
		{OS: machineOSOther, Architecture: machineArchitectureUnknown, TotalMemoryKnown: false, TotalMemoryBytes: 0},
		{OS: machineOSOther, Architecture: machineArchitecture(99), TotalMemoryKnown: true, TotalMemoryBytes: 16 * gib},
	}
	for i, facts := range tests {
		p, err := planAutomatic(facts)
		if err != errMachineFacts || p != (automaticPlan{}) {
			t.Fatalf("case %d = %#v, %v", i, p, err)
		}
		if err.Error() != "machine_facts_invalid" {
			t.Fatalf("case %d leaked facts: %q", i, err)
		}
	}
}

func TestPlanAutomaticKnownIneligibleHasNoActionableFields(t *testing.T) {
	tests := []struct {
		name        string
		facts       machineFacts
		reason      string
		fingerprint string
	}{
		{"other os", machineFacts{machineOSOther, machineArchitectureARM64, true, 32 * gib}, reasonOSIneligible, "40914d60e470e60a9adfa4cc58f0bb2ec5501a499f589f0ec2088a4557adfec8"},
		{"other architecture", machineFacts{machineOSDarwin, machineArchitectureOther, true, 32 * gib}, reasonArchitectureIneligible, "673a3c5e872f285c4be0b739e3aa6d1e3c07e6aed623ad79dbd18520c375e603"},
		{"low memory", validFacts(16*gib - 1), reasonMemoryBelowMinimum, "f2b6e197ffc67e0af45ce52107a0b2587164809f750ad3fa0c7c3b4385798e26"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := requirePlan(t, tt.facts)
			if p.eligibility != eligibilityIneligible || p.reason != tt.reason {
				t.Fatalf("classification = %q %q", p.eligibility, p.reason)
			}
			if p.runtime != (bootstrapRuntimePolicy{}) || p.model != (bootstrapModelPolicy{}) || p.limits != (generationLimits{}) || p.deadlinesSeconds != (bootstrapDeadlines{}) {
				t.Fatalf("ineligible plan has actionable fields: %#v", p)
			}
			fingerprint, err := p.fingerprint()
			if err != nil || fingerprint != tt.fingerprint {
				t.Fatalf("fingerprint = %q, %v", fingerprint, err)
			}
		})
	}
}

func TestPlanAutomaticNeverSelectsDisabledModel(t *testing.T) {
	for _, memory := range []uint64{1, 6 * gib, 16*gib - 1, 16 * gib, 24 * gib, 32*gib - 1, 32 * gib, math.MaxUint64} {
		p := requirePlan(t, validFacts(memory))
		if p.model.ID == "qwen3:1.7b" {
			t.Fatalf("selected disabled model at %d bytes", memory)
		}
	}
}

func TestAutomaticPlanCanonicalAndFingerprint(t *testing.T) {
	tests := []struct {
		name        string
		memory      uint64
		canonical   string
		fingerprint string
	}{
		{
			"4b", 16 * gib,
			`{"schema_version":1,"policy_fingerprint":"9df7c39f886c30f61f5a8200d50db42d828b26d335d865634aa56cf09ca1ac23","eligibility":"eligible","reason":"","model_id":"qwen3:4b","minimum_free_disk_bytes":8589934592}`,
			"73e198f2e1f4277df4f625f61a807caf47a6808fcec87a5462a0a2ee42d82afa",
		},
		{
			"8b", 32 * gib,
			`{"schema_version":1,"policy_fingerprint":"9df7c39f886c30f61f5a8200d50db42d828b26d335d865634aa56cf09ca1ac23","eligibility":"eligible","reason":"","model_id":"qwen3:8b","minimum_free_disk_bytes":15032385536}`,
			"6fd4d37586f64099887cdd4f6e7299b1f445fdd7765a10a0f13d7997dc1b4a87",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := requirePlan(t, validFacts(tt.memory))
			b, err := p.canonical()
			if err != nil || string(b) != tt.canonical || bytes.HasSuffix(b, []byte("\n")) {
				t.Fatalf("canonical = %q, %v", b, err)
			}
			fingerprint, err := p.fingerprint()
			if err != nil || fingerprint != tt.fingerprint {
				t.Fatalf("fingerprint = %q, %v", fingerprint, err)
			}
		})
	}
}

func TestAutomaticPlanRejectsMixedOrMutatedPayload(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*automaticPlan)
	}{
		{"policy fingerprint", func(p *automaticPlan) { p.policyFingerprint = "0" + p.policyFingerprint[1:] }},
		{"eligibility", func(p *automaticPlan) { p.eligibility = eligibilityIneligible }},
		{"eligible reason", func(p *automaticPlan) { p.reason = reasonOSIneligible }},
		{"runtime version", func(p *automaticPlan) { p.runtime.Version = "v0.35.2" }},
		{"runtime url", func(p *automaticPlan) { p.runtime.AssetURL += "?changed" }},
		{"runtime size", func(p *automaticPlan) { p.runtime.CompressedBytes++ }},
		{"runtime digest", func(p *automaticPlan) { p.runtime.SHA256 = "changed" }},
		{"runtime provenance", func(p *automaticPlan) { p.runtime.SHA256Provenance = "changed" }},
		{"model id", func(p *automaticPlan) { p.model.ID = "qwen3:8b" }},
		{"model digest", func(p *automaticPlan) { p.model.ManifestBodySHA256 = "changed" }},
		{"model provenance", func(p *automaticPlan) { p.model.SHA256Provenance = "changed" }},
		{"model floor", func(p *automaticPlan) { p.model.MinimumFreeDiskBytes++ }},
		{"model start", func(p *automaticPlan) { p.model.StartPolicy = "disabled" }},
		{"context limit", func(p *automaticPlan) { p.limits.ContextTokens++ }},
		{"output limit", func(p *automaticPlan) { p.limits.OutputTokens++ }},
		{"connect deadline", func(p *automaticPlan) { p.deadlinesSeconds.Connect++ }},
		{"progress deadline", func(p *automaticPlan) { p.deadlinesSeconds.NoProgress++ }},
		{"pull deadline", func(p *automaticPlan) { p.deadlinesSeconds.PullTotal++ }},
		{"warmup deadline", func(p *automaticPlan) { p.deadlinesSeconds.Warmup++ }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := requirePlan(t, validFacts(16*gib))
			tt.mutate(&p)
			if p.valid() {
				t.Fatal("mutated plan is valid")
			}
			if b, err := p.canonical(); err != errPlan || b != nil {
				t.Fatalf("canonical = %q, %v", b, err)
			}
			if fingerprint, err := p.fingerprint(); err != errPlan || fingerprint != "" {
				t.Fatalf("fingerprint = %q, %v", fingerprint, err)
			}
		})
	}
}

func TestAutomaticPlanRejectsActionableIneligiblePayload(t *testing.T) {
	p := requirePlan(t, machineFacts{machineOSOther, machineArchitectureARM64, true, 32 * gib})
	policy := compiledBootstrapPolicy()
	mutations := []func(*automaticPlan){
		func(p *automaticPlan) { p.runtime = policy.Runtime },
		func(p *automaticPlan) { p.model = policy.Models[1] },
		func(p *automaticPlan) { p.limits = policy.Limits },
		func(p *automaticPlan) { p.deadlinesSeconds = policy.DeadlinesSeconds },
		func(p *automaticPlan) { p.reason = "" },
	}
	for i, mutate := range mutations {
		candidate := p
		mutate(&candidate)
		if candidate.valid() {
			t.Fatalf("mutation %d retained valid ineligible plan", i)
		}
	}
}

func TestPlanAutomaticIsDeterministic(t *testing.T) {
	facts := validFacts(24 * gib)
	first := requirePlan(t, facts)
	for i := 0; i < 100; i++ {
		next := requirePlan(t, facts)
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("call %d changed plan", i)
		}
		firstBytes, firstErr := first.canonical()
		nextBytes, nextErr := next.canonical()
		if firstErr != nil || nextErr != nil || !bytes.Equal(firstBytes, nextBytes) {
			t.Fatalf("call %d changed canonical value", i)
		}
	}
}
