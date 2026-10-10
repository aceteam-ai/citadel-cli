package desktopmodel

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

const goldenBootstrapPolicy = `{"schema_version":1,"runtime":{"version":"v0.35.1","asset_url":"https://github.com/ollama/ollama/releases/download/v0.35.1/ollama-darwin.tgz","compressed_bytes":159625239,"sha256":"3137dbf28948ee844e0fb3e584d9b5de6879d73d9f0cb7eff3ad64930601d307","sha256_provenance":"github_release_asset_api_metadata"},"models":[{"id":"qwen3:1.7b","manifest_body_sha256":"8f68893c685c3ddff2aa3fffce2aa60a30bb2da65ca488b61fff134a4d1730e7","sha256_provenance":"locally_hashed_registry_manifest_body","minimum_free_disk_bytes":6442450944,"start_policy":"disabled"},{"id":"qwen3:4b","manifest_body_sha256":"359d7dd4bcdab3d86b87d73ac27966f4dbb9f5efdfcc75d34a8764a09474fae7","sha256_provenance":"locally_hashed_registry_manifest_body","minimum_free_disk_bytes":8589934592,"start_policy":"automatic"},{"id":"qwen3:8b","manifest_body_sha256":"500a1f067a9f782620b40bee6f7b0c89e17ae61f686b92c24933e4ca4b2b8b41","sha256_provenance":"locally_hashed_registry_manifest_body","minimum_free_disk_bytes":15032385536,"start_policy":"automatic"}],"automatic_machine":{"os":"darwin","architecture":"arm64"},"automatic_tiers":[{"minimum_memory_bytes":17179869184,"maximum_memory_bytes_exclusive":34359738368,"model_id":"qwen3:4b"},{"minimum_memory_bytes":34359738368,"maximum_memory_bytes_exclusive":0,"model_id":"qwen3:8b"}],"limits":{"context_tokens":4096,"output_tokens":256},"deadlines_seconds":{"connect":30,"no_progress":120,"pull_total":1800,"warmup":120},"evidence_limits":{"runtime_archive":"metadata_only_not_downloaded","model_layers":"not_downloaded","license":"descriptor_metadata_only_not_reviewed","native":"not_inspected_or_executed"}}`

const goldenBootstrapPolicyFingerprint = "9df7c39f886c30f61f5a8200d50db42d828b26d335d865634aa56cf09ca1ac23"

func TestBootstrapPolicyCanonicalAndFingerprint(t *testing.T) {
	p := compiledBootstrapPolicy()
	b, err := p.canonical()
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if string(b) != goldenBootstrapPolicy {
		t.Fatalf("canonical policy mismatch\n got: %s\nwant: %s", b, goldenBootstrapPolicy)
	}
	if bytes.HasSuffix(b, []byte("\n")) {
		t.Fatal("canonical policy has trailing newline")
	}
	fingerprint, err := p.fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if fingerprint != goldenBootstrapPolicyFingerprint {
		t.Fatalf("fingerprint = %q, want %q", fingerprint, goldenBootstrapPolicyFingerprint)
	}
}

func TestBootstrapPolicyRatifiedValuesAndUnits(t *testing.T) {
	p := compiledBootstrapPolicy()
	if gib != 1073741824 {
		t.Fatalf("gib = %d", gib)
	}
	if p.Runtime.CompressedBytes != 159625239 {
		t.Fatalf("compressed bytes = %d", p.Runtime.CompressedBytes)
	}
	wantDisk := [3]uint64{6442450944, 8589934592, 15032385536}
	for i, want := range wantDisk {
		if p.Models[i].MinimumFreeDiskBytes != want {
			t.Fatalf("model %d floor = %d, want %d", i, p.Models[i].MinimumFreeDiskBytes, want)
		}
	}
	if p.AutomaticTiers[0].MinimumMemoryBytes != 17179869184 || p.AutomaticTiers[0].MaximumMemoryBytesExclusive != 34359738368 || p.AutomaticTiers[1].MinimumMemoryBytes != 34359738368 || p.AutomaticTiers[1].MaximumMemoryBytesExclusive != 0 {
		t.Fatalf("unexpected automatic tiers: %#v", p.AutomaticTiers)
	}
	if p.Limits != (generationLimits{ContextTokens: 4096, OutputTokens: 256}) {
		t.Fatalf("unexpected limits: %#v", p.Limits)
	}
	if p.DeadlinesSeconds != (bootstrapDeadlines{Connect: 30, NoProgress: 120, PullTotal: 1800, Warmup: 120}) {
		t.Fatalf("unexpected deadlines: %#v", p.DeadlinesSeconds)
	}
}

func TestBootstrapPolicyRejectsEveryMutation(t *testing.T) {
	type mutationCase struct {
		name   string
		mutate func(*bootstrapPolicyManifest)
	}
	tests := []mutationCase{
		{"schema zero", func(p *bootstrapPolicyManifest) { p.SchemaVersion = 0 }},
		{"schema", func(p *bootstrapPolicyManifest) { p.SchemaVersion = 2 }},
		{"runtime version", func(p *bootstrapPolicyManifest) { p.Runtime.Version = "v0.35.2" }},
		{"runtime url", func(p *bootstrapPolicyManifest) { p.Runtime.AssetURL += "?changed" }},
		{"runtime size", func(p *bootstrapPolicyManifest) { p.Runtime.CompressedBytes++ }},
		{"runtime digest", func(p *bootstrapPolicyManifest) { p.Runtime.SHA256 = strings.Repeat("0", 64) }},
		{"runtime digest uppercase", func(p *bootstrapPolicyManifest) { p.Runtime.SHA256 = strings.ToUpper(p.Runtime.SHA256) }},
		{"runtime digest short", func(p *bootstrapPolicyManifest) { p.Runtime.SHA256 = p.Runtime.SHA256[:63] }},
		{"runtime digest nonhex", func(p *bootstrapPolicyManifest) { p.Runtime.SHA256 = strings.Repeat("g", 64) }},
		{"runtime provenance", func(p *bootstrapPolicyManifest) { p.Runtime.SHA256Provenance = "downloaded" }},
		{"model order", func(p *bootstrapPolicyManifest) { p.Models[0], p.Models[1] = p.Models[1], p.Models[0] }},
		{"duplicate model", func(p *bootstrapPolicyManifest) { p.Models[2] = p.Models[1] }},
		{"missing model", func(p *bootstrapPolicyManifest) { p.Models[1] = bootstrapModelPolicy{} }},
		{"model id", func(p *bootstrapPolicyManifest) { p.Models[0].ID = "qwen3:2b" }},
		{"model digest", func(p *bootstrapPolicyManifest) { p.Models[1].ManifestBodySHA256 = strings.Repeat("f", 64) }},
		{"model digest uppercase", func(p *bootstrapPolicyManifest) {
			p.Models[2].ManifestBodySHA256 = strings.ToUpper(p.Models[2].ManifestBodySHA256)
		}},
		{"model provenance", func(p *bootstrapPolicyManifest) { p.Models[2].SHA256Provenance = "layer_hash" }},
		{"model floor", func(p *bootstrapPolicyManifest) { p.Models[1].MinimumFreeDiskBytes = 8_000_000_000 }},
		{"model floor overflow", func(p *bootstrapPolicyManifest) { p.Models[1].MinimumFreeDiskBytes = ^uint64(0) }},
		{"1.7b automatic", func(p *bootstrapPolicyManifest) { p.Models[0].StartPolicy = "automatic" }},
		{"4b disabled", func(p *bootstrapPolicyManifest) { p.Models[1].StartPolicy = "disabled" }},
		{"8b disabled", func(p *bootstrapPolicyManifest) { p.Models[2].StartPolicy = "disabled" }},
		{"machine os", func(p *bootstrapPolicyManifest) { p.AutomaticMachine.OS = "linux" }},
		{"machine architecture", func(p *bootstrapPolicyManifest) { p.AutomaticMachine.Architecture = "amd64" }},
		{"tier order", func(p *bootstrapPolicyManifest) {
			p.AutomaticTiers[0], p.AutomaticTiers[1] = p.AutomaticTiers[1], p.AutomaticTiers[0]
		}},
		{"tier decimal minimum", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[0].MinimumMemoryBytes = 16_000_000_000 }},
		{"tier gap", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[0].MaximumMemoryBytesExclusive-- }},
		{"tier bounds reversed", func(p *bootstrapPolicyManifest) {
			p.AutomaticTiers[0].MaximumMemoryBytesExclusive = p.AutomaticTiers[0].MinimumMemoryBytes - 1
		}},
		{"tier minimum overflow", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[0].MinimumMemoryBytes = ^uint64(0) }},
		{"tier overlap", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[1].MinimumMemoryBytes-- }},
		{"nonfinal open upper", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[0].MaximumMemoryBytesExclusive = 0 }},
		{"final upper", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[1].MaximumMemoryBytesExclusive = ^uint64(0) }},
		{"tier model", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[0].ModelID = "qwen3:8b" }},
		{"context cap", func(p *bootstrapPolicyManifest) { p.Limits.ContextTokens++ }},
		{"output cap", func(p *bootstrapPolicyManifest) { p.Limits.OutputTokens++ }},
		{"connect deadline", func(p *bootstrapPolicyManifest) { p.DeadlinesSeconds.Connect++ }},
		{"progress deadline", func(p *bootstrapPolicyManifest) { p.DeadlinesSeconds.NoProgress++ }},
		{"pull deadline", func(p *bootstrapPolicyManifest) { p.DeadlinesSeconds.PullTotal++ }},
		{"warmup deadline", func(p *bootstrapPolicyManifest) { p.DeadlinesSeconds.Warmup++ }},
		{"runtime evidence", func(p *bootstrapPolicyManifest) { p.EvidenceLimits.RuntimeArchive = "downloaded" }},
		{"model evidence", func(p *bootstrapPolicyManifest) { p.EvidenceLimits.ModelLayers = "downloaded" }},
		{"license evidence", func(p *bootstrapPolicyManifest) { p.EvidenceLimits.License = "accepted" }},
		{"native evidence", func(p *bootstrapPolicyManifest) { p.EvidenceLimits.Native = "executed" }},
	}
	for i := range compiledBootstrapPolicy().Models {
		index := i
		prefix := "model " + strconv.Itoa(index) + " "
		tests = append(tests,
			mutationCase{prefix + "id", func(p *bootstrapPolicyManifest) { p.Models[index].ID += "changed" }},
			mutationCase{prefix + "digest", func(p *bootstrapPolicyManifest) { p.Models[index].ManifestBodySHA256 = strings.Repeat("g", 64) }},
			mutationCase{prefix + "provenance", func(p *bootstrapPolicyManifest) { p.Models[index].SHA256Provenance = "changed" }},
			mutationCase{prefix + "floor", func(p *bootstrapPolicyManifest) { p.Models[index].MinimumFreeDiskBytes++ }},
			mutationCase{prefix + "start policy", func(p *bootstrapPolicyManifest) { p.Models[index].StartPolicy = "changed" }},
		)
	}
	for i := range compiledBootstrapPolicy().AutomaticTiers {
		index := i
		prefix := "tier " + strconv.Itoa(index) + " "
		tests = append(tests,
			mutationCase{prefix + "minimum", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[index].MinimumMemoryBytes++ }},
			mutationCase{prefix + "maximum", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[index].MaximumMemoryBytesExclusive++ }},
			mutationCase{prefix + "model", func(p *bootstrapPolicyManifest) { p.AutomaticTiers[index].ModelID += "changed" }},
		)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := compiledBootstrapPolicy()
			tt.mutate(&p)
			if p.valid() {
				t.Fatal("mutated policy is valid")
			}
			if b, err := p.canonical(); err != errBootstrapPolicy || b != nil {
				t.Fatalf("canonical = %q, %v", b, err)
			}
			if fingerprint, err := p.fingerprint(); err != errBootstrapPolicy || fingerprint != "" {
				t.Fatalf("fingerprint = %q, %v", fingerprint, err)
			}
		})
	}
}

func TestBootstrapPolicyProvenanceDoesNotOverclaim(t *testing.T) {
	b, err := compiledBootstrapPolicy().canonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"minimum_macos", "deployment_target", "advertisement", "remaining_layer_reserve",
		"aggregate_layer", `"licensed"`, `"native_approved"`, `"ready"`, `"serving"`,
		`"installed"`, `"timestamp"`, `"override"`,
	} {
		if bytes.Contains(b, []byte(forbidden)) {
			t.Fatalf("canonical policy contains forbidden claim %q", forbidden)
		}
	}
}

func TestBootstrapPolicyConstructorsAreIndependent(t *testing.T) {
	first := compiledBootstrapPolicy()
	first.Runtime.Version = "mutated"
	first.Models[0].ID = "mutated"
	first.AutomaticTiers[0].MinimumMemoryBytes = 1

	second := compiledBootstrapPolicy()
	if !second.valid() {
		t.Fatal("later policy was changed through earlier value")
	}
	fingerprint, err := second.fingerprint()
	if err != nil || fingerprint != goldenBootstrapPolicyFingerprint {
		t.Fatalf("later fingerprint = %q, %v", fingerprint, err)
	}
}
