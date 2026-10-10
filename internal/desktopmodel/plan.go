package desktopmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	errMachineFacts foundationError = "machine_facts_invalid"
	errPlan         foundationError = "plan_invalid"

	eligibilityEligible   = "eligible"
	eligibilityIneligible = "ineligible"

	reasonOSIneligible           = "os_ineligible"
	reasonArchitectureIneligible = "architecture_ineligible"
	reasonMemoryBelowMinimum     = "memory_below_minimum"
)

type machineOS uint8

const (
	machineOSUnknown machineOS = iota
	machineOSDarwin
	machineOSOther
)

type machineArchitecture uint8

const (
	machineArchitectureUnknown machineArchitecture = iota
	machineArchitectureARM64
	machineArchitectureOther
)

type machineFacts struct {
	OS               machineOS
	Architecture     machineArchitecture
	TotalMemoryKnown bool
	TotalMemoryBytes uint64
}

func (f machineFacts) valid() bool {
	osValid := f.OS == machineOSDarwin || f.OS == machineOSOther
	architectureValid := f.Architecture == machineArchitectureARM64 || f.Architecture == machineArchitectureOther
	return osValid && architectureValid && f.TotalMemoryKnown && f.TotalMemoryBytes > 0
}

type automaticPlan struct {
	policyFingerprint string
	eligibility       string
	reason            string
	runtime           bootstrapRuntimePolicy
	model             bootstrapModelPolicy
	limits            generationLimits
	deadlinesSeconds  bootstrapDeadlines
}

type canonicalPlanRecord struct {
	SchemaVersion        uint64 `json:"schema_version"`
	PolicyFingerprint    string `json:"policy_fingerprint"`
	Eligibility          string `json:"eligibility"`
	Reason               string `json:"reason"`
	ModelID              string `json:"model_id"`
	MinimumFreeDiskBytes uint64 `json:"minimum_free_disk_bytes"`
}

func planAutomatic(facts machineFacts) (automaticPlan, error) {
	if !facts.valid() {
		return automaticPlan{}, errMachineFacts
	}
	policy := compiledBootstrapPolicy()
	policyFingerprint, err := policy.fingerprint()
	if err != nil {
		return automaticPlan{}, errPlan
	}
	if facts.OS != machineOSDarwin {
		return ineligiblePlan(policyFingerprint, reasonOSIneligible), nil
	}
	if facts.Architecture != machineArchitectureARM64 {
		return ineligiblePlan(policyFingerprint, reasonArchitectureIneligible), nil
	}
	if facts.TotalMemoryBytes < policy.AutomaticTiers[0].MinimumMemoryBytes {
		return ineligiblePlan(policyFingerprint, reasonMemoryBelowMinimum), nil
	}

	model := policy.Models[1]
	if facts.TotalMemoryBytes >= policy.AutomaticTiers[1].MinimumMemoryBytes {
		model = policy.Models[2]
	}
	return automaticPlan{
		policyFingerprint: policyFingerprint,
		eligibility:       eligibilityEligible,
		runtime:           policy.Runtime,
		model:             model,
		limits:            policy.Limits,
		deadlinesSeconds:  policy.DeadlinesSeconds,
	}, nil
}

func ineligiblePlan(policyFingerprint, reason string) automaticPlan {
	return automaticPlan{
		policyFingerprint: policyFingerprint,
		eligibility:       eligibilityIneligible,
		reason:            reason,
	}
}

func (p automaticPlan) valid() bool {
	policy := compiledBootstrapPolicy()
	policyFingerprint, err := policy.fingerprint()
	if err != nil || p.policyFingerprint != policyFingerprint {
		return false
	}

	if p.eligibility == eligibilityEligible {
		modelValid := p.model == policy.Models[1] || p.model == policy.Models[2]
		return p.reason == "" && modelValid && p.runtime == policy.Runtime && p.limits == policy.Limits && p.deadlinesSeconds == policy.DeadlinesSeconds
	}
	if p.eligibility != eligibilityIneligible {
		return false
	}
	reasonValid := p.reason == reasonOSIneligible || p.reason == reasonArchitectureIneligible || p.reason == reasonMemoryBelowMinimum
	return reasonValid && p.runtime == (bootstrapRuntimePolicy{}) && p.model == (bootstrapModelPolicy{}) && p.limits == (generationLimits{}) && p.deadlinesSeconds == (bootstrapDeadlines{})
}

func (p automaticPlan) canonical() ([]byte, error) {
	if !p.valid() {
		return nil, errPlan
	}
	record := canonicalPlanRecord{
		SchemaVersion:     1,
		PolicyFingerprint: p.policyFingerprint,
		Eligibility:       p.eligibility,
		Reason:            p.reason,
	}
	if p.eligibility == eligibilityEligible {
		record.ModelID = p.model.ID
		record.MinimumFreeDiskBytes = p.model.MinimumFreeDiskBytes
	}
	b, err := json.Marshal(record)
	if err != nil {
		return nil, errPlan
	}
	return b, nil
}

func (p automaticPlan) fingerprint() (string, error) {
	b, err := p.canonical()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
