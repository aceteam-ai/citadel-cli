package desktopmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const gib = uint64(1) << 30

const errBootstrapPolicy foundationError = "bootstrap_policy_invalid"

type bootstrapRuntimePolicy struct {
	Version          string `json:"version"`
	AssetURL         string `json:"asset_url"`
	CompressedBytes  uint64 `json:"compressed_bytes"`
	SHA256           string `json:"sha256"`
	SHA256Provenance string `json:"sha256_provenance"`
}

type bootstrapModelPolicy struct {
	ID                   string `json:"id"`
	ManifestBodySHA256   string `json:"manifest_body_sha256"`
	SHA256Provenance     string `json:"sha256_provenance"`
	MinimumFreeDiskBytes uint64 `json:"minimum_free_disk_bytes"`
	StartPolicy          string `json:"start_policy"`
}

type automaticMachinePolicy struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type automaticTierPolicy struct {
	MinimumMemoryBytes          uint64 `json:"minimum_memory_bytes"`
	MaximumMemoryBytesExclusive uint64 `json:"maximum_memory_bytes_exclusive"`
	ModelID                     string `json:"model_id"`
}

type generationLimits struct {
	ContextTokens uint64 `json:"context_tokens"`
	OutputTokens  uint64 `json:"output_tokens"`
}

type bootstrapDeadlines struct {
	Connect    uint64 `json:"connect"`
	NoProgress uint64 `json:"no_progress"`
	PullTotal  uint64 `json:"pull_total"`
	Warmup     uint64 `json:"warmup"`
}

type evidenceLimits struct {
	RuntimeArchive string `json:"runtime_archive"`
	ModelLayers    string `json:"model_layers"`
	License        string `json:"license"`
	Native         string `json:"native"`
}

type bootstrapPolicyManifest struct {
	SchemaVersion    uint64                  `json:"schema_version"`
	Runtime          bootstrapRuntimePolicy  `json:"runtime"`
	Models           [3]bootstrapModelPolicy `json:"models"`
	AutomaticMachine automaticMachinePolicy  `json:"automatic_machine"`
	AutomaticTiers   [2]automaticTierPolicy  `json:"automatic_tiers"`
	Limits           generationLimits        `json:"limits"`
	DeadlinesSeconds bootstrapDeadlines      `json:"deadlines_seconds"`
	EvidenceLimits   evidenceLimits          `json:"evidence_limits"`
}

func compiledBootstrapPolicy() bootstrapPolicyManifest {
	return bootstrapPolicyManifest{
		SchemaVersion: 1,
		Runtime: bootstrapRuntimePolicy{
			Version:          "v0.35.1",
			AssetURL:         "https://github.com/ollama/ollama/releases/download/v0.35.1/ollama-darwin.tgz",
			CompressedBytes:  159625239,
			SHA256:           "3137dbf28948ee844e0fb3e584d9b5de6879d73d9f0cb7eff3ad64930601d307",
			SHA256Provenance: "github_release_asset_api_metadata",
		},
		Models: [3]bootstrapModelPolicy{
			{
				ID:                   "qwen3:1.7b",
				ManifestBodySHA256:   "8f68893c685c3ddff2aa3fffce2aa60a30bb2da65ca488b61fff134a4d1730e7",
				SHA256Provenance:     "locally_hashed_registry_manifest_body",
				MinimumFreeDiskBytes: 6 * gib,
				StartPolicy:          "disabled",
			},
			{
				ID:                   "qwen3:4b",
				ManifestBodySHA256:   "359d7dd4bcdab3d86b87d73ac27966f4dbb9f5efdfcc75d34a8764a09474fae7",
				SHA256Provenance:     "locally_hashed_registry_manifest_body",
				MinimumFreeDiskBytes: 8 * gib,
				StartPolicy:          "automatic",
			},
			{
				ID:                   "qwen3:8b",
				ManifestBodySHA256:   "500a1f067a9f782620b40bee6f7b0c89e17ae61f686b92c24933e4ca4b2b8b41",
				SHA256Provenance:     "locally_hashed_registry_manifest_body",
				MinimumFreeDiskBytes: 14 * gib,
				StartPolicy:          "automatic",
			},
		},
		AutomaticMachine: automaticMachinePolicy{OS: "darwin", Architecture: "arm64"},
		AutomaticTiers: [2]automaticTierPolicy{
			{MinimumMemoryBytes: 16 * gib, MaximumMemoryBytesExclusive: 32 * gib, ModelID: "qwen3:4b"},
			{MinimumMemoryBytes: 32 * gib, MaximumMemoryBytesExclusive: 0, ModelID: "qwen3:8b"},
		},
		Limits:           generationLimits{ContextTokens: 4096, OutputTokens: 256},
		DeadlinesSeconds: bootstrapDeadlines{Connect: 30, NoProgress: 120, PullTotal: 1800, Warmup: 120},
		EvidenceLimits: evidenceLimits{
			RuntimeArchive: "metadata_only_not_downloaded",
			ModelLayers:    "not_downloaded",
			License:        "descriptor_metadata_only_not_reviewed",
			Native:         "not_inspected_or_executed",
		},
	}
}

func (p bootstrapPolicyManifest) valid() bool {
	return p == compiledBootstrapPolicy()
}

func (p bootstrapPolicyManifest) canonical() ([]byte, error) {
	if !p.valid() {
		return nil, errBootstrapPolicy
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, errBootstrapPolicy
	}
	return b, nil
}

func (p bootstrapPolicyManifest) fingerprint() (string, error) {
	b, err := p.canonical()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
