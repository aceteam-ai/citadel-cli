package engine

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/services"
)

// chatBackends is the exact set of backends internal/worker/llm_inference.go's
// Execute() routing switch dispatches (verified against that switch directly
// at doc-writing time). Used ONLY for the Dialect assertion below: Dialect is
// new synthesis with no source table to check against (unlike ReadyPath, a
// real map lookup, and LoadEstimate, a total function -- both checked
// unconditionally above), so this is the one field where "not a chat
// backend" legitimately means "must be the zero value".
var chatBackends = map[string]bool{
	"vllm":          true,
	"sglang":        true,
	"ollama":        true,
	"llamacpp":      true,
	"bonsai":        true,
	"unlimited-ocr": true,
}

// TestRegistryEquivalence is the safety net for this whole slice: for every
// engine services.ServiceMap knows about, assert the registry's EngineSpec
// equals exactly what the existing tables return today. This is what lets
// later slices delete/migrate a table with confidence, per design doc §3's
// "don't delete old tables until proven equivalent" rule -- this test is that
// proof, kept passing across every future migration step.
func TestRegistryEquivalence(t *testing.T) {
	reg := Default()

	// Registry must cover exactly services.ServiceMap -- no more, no fewer.
	all := reg.All()
	if len(all) != len(services.ServiceMap) {
		t.Fatalf("registry has %d engines, services.ServiceMap has %d", len(all), len(services.ServiceMap))
	}

	for name := range services.ServiceMap {
		name := name
		t.Run(name, func(t *testing.T) {
			e, ok := reg.Lookup(name)
			if !ok {
				t.Fatalf("Lookup(%q) not found in registry", name)
			}
			spec := e.Spec()

			if e.Name() != name {
				t.Errorf("Name() = %q, want %q", e.Name(), name)
			}
			if spec.Name != name {
				t.Errorf("Spec().Name = %q, want %q", spec.Name, name)
			}
			if e.Kind() != spec.Kind {
				t.Errorf("Kind() = %q, Spec().Kind = %q, want equal", e.Kind(), spec.Kind)
			}

			// HostPort: services/internal.status's own resolution (registry
			// lookup, then compose-parse fallback) -- the exact function the
			// heartbeat itself would call.
			wantHostPort := status.ManagedEngineHostPort(name)
			if spec.HostPort != wantHostPort {
				t.Errorf("HostPort = %d, want %d (status.ManagedEngineHostPort)", spec.HostPort, wantHostPort)
			}

			// HostPortEnvVar: services/ports.go's serviceHostPortEnv.
			wantEnvVar, _ := services.HostPortEnvVarName(name)
			if spec.HostPortEnvVar != wantEnvVar {
				t.Errorf("HostPortEnvVar = %q, want %q (services.HostPortEnvVarName)", spec.HostPortEnvVar, wantEnvVar)
			}

			// CacheDir/CacheFamily: services.EngineCacheDirs.
			wantCache, wantCacheOK := services.EngineCacheDirs[name]
			if wantCacheOK {
				if spec.CacheDir != wantCache.Dir {
					t.Errorf("CacheDir = %q, want %q (services.EngineCacheDirs)", spec.CacheDir, wantCache.Dir)
				}
				if spec.CacheFamily != wantCache.Family {
					t.Errorf("CacheFamily = %q, want %q (services.EngineCacheDirs)", spec.CacheFamily, wantCache.Family)
				}
			} else if spec.CacheDir != "" || spec.CacheFamily != "" {
				t.Errorf("CacheDir/CacheFamily = %q/%q, want empty (no services.EngineCacheDirs entry)", spec.CacheDir, spec.CacheFamily)
			}

			// ModelEnvVar: status.EngineModelEnvVars.
			wantModelEnvVar := status.EngineModelEnvVars(name)
			if !stringSlicesEqual(spec.ModelEnvVar, wantModelEnvVar) {
				t.Errorf("ModelEnvVar = %v, want %v (status.EngineModelEnvVars)", spec.ModelEnvVar, wantModelEnvVar)
			}

			// DefaultModel: status.EngineDefaultModel, pointer-vs-absence must
			// match exactly (citadel #685 §1a's whole point).
			wantDefault, wantDefaultOK := status.EngineDefaultModel(name)
			switch {
			case wantDefaultOK && spec.DefaultModel == nil:
				t.Errorf("DefaultModel = nil, want %q (status.EngineDefaultModel has an entry)", wantDefault)
			case !wantDefaultOK && spec.DefaultModel != nil:
				t.Errorf("DefaultModel = %q, want nil (status.EngineDefaultModel has no entry)", *spec.DefaultModel)
			case wantDefaultOK && spec.DefaultModel != nil && *spec.DefaultModel != wantDefault:
				t.Errorf("DefaultModel = %q, want %q", *spec.DefaultModel, wantDefault)
			}

			// VRAMEstimateMB: status.EngineVRAMEstimateMB.
			wantVRAM := status.EngineVRAMEstimateMB(name)
			if spec.VRAMEstimateMB != wantVRAM {
				t.Errorf("VRAMEstimateMB = %d, want %d (status.EngineVRAMEstimateMB)", spec.VRAMEstimateMB, wantVRAM)
			}

			// IdleCapable/EmbeddingCapable/ManagedProbe: membership in the
			// corresponding status accessor list.
			if want := stringInSlice(status.IdleCapableEngines(), name); spec.IdleCapable != want {
				t.Errorf("IdleCapable = %v, want %v (status.IdleCapableEngines)", spec.IdleCapable, want)
			}
			if want := stringInSlice(status.EmbeddingProbeServices(), name); spec.EmbeddingCapable != want {
				t.Errorf("EmbeddingCapable = %v, want %v (status.EmbeddingProbeServices)", spec.EmbeddingCapable, want)
			}
			if want := stringInSlice(status.ManagedProbeEngines(), name); spec.ManagedProbe != want {
				t.Errorf("ManagedProbe = %v, want %v (status.ManagedProbeEngines)", spec.ManagedProbe, want)
			}

			// MetricsPort: services.InferenceMetricsPorts().
			wantMetricsPort := services.InferenceMetricsPorts()[name]
			if spec.MetricsPort != wantMetricsPort {
				t.Errorf("MetricsPort = %d, want %d (services.InferenceMetricsPorts)", spec.MetricsPort, wantMetricsPort)
			}

			// SelfProvisioning: internal/jobs.IsSelfProvisioningEngine.
			wantSelfProvisioning := jobs.IsSelfProvisioningEngine(name)
			if spec.SelfProvisioning != wantSelfProvisioning {
				t.Errorf("SelfProvisioning = %v, want %v (jobs.IsSelfProvisioningEngine)", spec.SelfProvisioning, wantSelfProvisioning)
			}

			// ReadyPath: internal/worker.EngineReadyPath. An engine with no
			// entry in the real table must translate to "".
			wantReadyPath, _ := worker.EngineReadyPath(name)
			if spec.ReadyPath != wantReadyPath {
				t.Errorf("ReadyPath = %q, want %q (worker.EngineReadyPath)", spec.ReadyPath, wantReadyPath)
			}

			// LoadEstimate: internal/worker.DefaultLoadEstimate. That function
			// is a switch WITH A DEFAULT CASE -- a total function, so there is
			// no "absent" value to carve out here (unlike ReadyPath, a real
			// map lookup with genuine absences). Checked unconditionally for
			// all twelve engines, including the six with no named case: the
			// real switch still returns a value (60s) for them, and this
			// spec's LoadEstimate must equal exactly that, not zero.
			wantLoadEstimate := worker.DefaultLoadEstimate(name)
			if spec.LoadEstimate != wantLoadEstimate {
				t.Errorf("LoadEstimate = %v, want %v (worker.DefaultLoadEstimate)", spec.LoadEstimate, wantLoadEstimate)
			}

			// Dialect: new synthesis (design doc §1e/§2), not translated from
			// an existing table -- only assert the structural rule this
			// package's own dialectByEngine encodes: a non-chat-backend
			// engine carries the zero value.
			if !chatBackends[name] && spec.Dialect != "" {
				t.Errorf("Dialect = %q, want \"\" (not an llm_inference backend)", spec.Dialect)
			}
		})
	}
}

// TestOnlyOllamaIsNativeProcess pins the one EngineKind fact this package
// asserts on its own authority (no existing table encodes "compose vs.
// native" today) -- ollama is the only services.ServiceMap engine not
// managed via docker compose (design doc §2's coverage table).
func TestOnlyOllamaIsNativeProcess(t *testing.T) {
	for _, e := range Default().All() {
		wantNative := e.Name() == "ollama"
		gotNative := e.Kind() == NativeProcess
		if gotNative != wantNative {
			t.Errorf("%s: Kind() = %q, want native=%v", e.Name(), e.Kind(), wantNative)
		}
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
