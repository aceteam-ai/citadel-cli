// internal/servicediag/vram.go
package servicediag

import "fmt"

// VRAMFitCheckName is the PreflightCheck.Name for the VRAM-fit check.
const VRAMFitCheckName = "vram_fit"

// VRAMFitCheck decides whether a service's declared VRAM need fits the node's
// current free VRAM, degrading to VerdictUnknown when either signal is
// unavailable -- it never fabricates a verdict from a missing input (citadel
// #852's explicit guardrail: a NOT-running service's "need" may legitimately
// be unknown).
func VRAMFitCheck(freeBytes uint64, freeKnown bool, needMB int, needKnown bool) PreflightCheck {
	switch {
	case !freeKnown && !needKnown:
		return PreflightCheck{Name: VRAMFitCheckName, Verdict: VerdictUnknown,
			Detail: "free VRAM and this service's declared VRAM need are both unavailable"}
	case !freeKnown:
		return PreflightCheck{Name: VRAMFitCheckName, Verdict: VerdictUnknown,
			Detail: "free VRAM is unavailable (no GPU detected, or nvidia-smi did not report)"}
	case !needKnown:
		return PreflightCheck{Name: VRAMFitCheckName, Verdict: VerdictUnknown,
			Detail: fmt.Sprintf("this service's declared VRAM need is unknown; %s free", gbString(freeBytes))}
	}

	needBytes := uint64(needMB) * 1024 * 1024
	if freeBytes >= needBytes {
		return PreflightCheck{Name: VRAMFitCheckName, Verdict: VerdictOK,
			Detail: fmt.Sprintf("%s free >= ~%s needed", gbString(freeBytes), gbString(needBytes))}
	}
	return PreflightCheck{Name: VRAMFitCheckName, Verdict: VerdictFail,
		Detail: fmt.Sprintf("only %s free < ~%s needed", gbString(freeBytes), gbString(needBytes))}
}

func gbString(b uint64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
}
