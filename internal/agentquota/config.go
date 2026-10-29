package agentquota

// Harness is a trusted policy assertion, not evidence of native harness identity.
// The future admission adapter must bind it to the actual lane in the outer CAS.
type Harness string

const (
	HarnessCodex      Harness = "codex"
	HarnessClaudeCode Harness = "claude-code"

	MaxCodexTurnsPerWindow  = 48
	MaxClaudeTurnsPerWindow = 10
	MaxActiveTurnsPerLane   = 1
)

type TurnLimits struct {
	MaxTurns       uint32
	MaxTurnMillis  uint64
	MaxActiveTurns uint32
}

// HarnessConfig may lower the approved per-harness limits. Its zero value is
// invalid, not an activating default or a way to discard existing quota debt.
type HarnessConfig struct {
	Codex      TurnLimits
	ClaudeCode TurnLimits
}

// DefaultHarnessConfig supplies only limits. It creates no lane, enables none,
// and grants no permission to reserve or launch a real turn.
func DefaultHarnessConfig() HarnessConfig {
	return HarnessConfig{
		Codex:      TurnLimits{MaxTurns: MaxCodexTurnsPerWindow, MaxTurnMillis: MaxTurnMillis, MaxActiveTurns: MaxActiveTurnsPerLane},
		ClaudeCode: TurnLimits{MaxTurns: MaxClaudeTurnsPerWindow, MaxTurnMillis: MaxTurnMillis, MaxActiveTurns: MaxActiveTurnsPerLane},
	}
}

func harnessMaximum(h Harness) uint32 {
	switch h {
	case HarnessCodex:
		return MaxCodexTurnsPerWindow
	case HarnessClaudeCode:
		return MaxClaudeTurnsPerWindow
	default:
		return 0
	}
}

func validLimits(limits TurnLimits, maximum uint32) bool {
	return limits.MaxTurns > 0 && limits.MaxTurns <= maximum && limits.MaxTurnMillis > 0 && limits.MaxTurnMillis <= MaxTurnMillis && limits.MaxActiveTurns == MaxActiveTurnsPerLane
}

func (config HarnessConfig) valid() bool {
	return validLimits(config.Codex, MaxCodexTurnsPerWindow) && validLimits(config.ClaudeCode, MaxClaudeTurnsPerWindow)
}

func (config HarnessConfig) limits(h Harness) TurnLimits {
	switch h {
	case HarnessCodex:
		return config.Codex
	case HarnessClaudeCode:
		return config.ClaudeCode
	default:
		return TurnLimits{}
	}
}

// effectiveLane retains per-lane lowering and applies any tighter current
// harness configuration. Increasing either cannot extend an existing charge.
func (policy Policy) effectiveLane(laneID string) (LanePolicy, bool) {
	for _, lane := range policy.Lanes {
		if lane.LaneID != laneID {
			continue
		}
		limits := policy.Harnesses.limits(lane.Harness)
		if limits.MaxTurns < lane.MaxTurns {
			lane.MaxTurns = limits.MaxTurns
		}
		if limits.MaxTurnMillis < lane.MaxTurnMillis {
			lane.MaxTurnMillis = limits.MaxTurnMillis
		}
		return lane, true
	}
	return LanePolicy{}, false
}
