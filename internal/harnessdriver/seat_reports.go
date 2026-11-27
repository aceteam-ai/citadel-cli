package harnessdriver

import (
	"encoding/json"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/seatquota"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// SeatQuota is the additive report-only quota payload. The supplied seat,
// provenance and clock are assertions, not verified identity or admission.
// Legacy Quota remains unchanged for reducers without a trusted seat binding.
type SeatQuota struct {
	Quota
	SeatRef               string           `json:"seat_ref"`
	WindowMinutes         int              `json:"window_minutes,omitempty"`
	Scope                 string           `json:"scope"`
	Model                 string           `json:"model,omitempty"`
	Reached               *bool            `json:"reached"`
	Reason                string           `json:"reason,omitempty"`
	Overage               *bool            `json:"overage"`
	ResetCreditsAvailable *int             `json:"reset_credits_available"`
	Source                seatquota.Source `json:"source"`
	Confidence            string           `json:"confidence"`
	AdapterVersion        string           `json:"adapter_version"`
	ObservedAt            time.Time        `json:"observed_at"`
}

// ProjectSeatQuota copies a supplied observation into the candidate contract.
// It performs no read, scheduling, reset consumption or routing. It deliberately
// omits provider detail and never treats a bucket identifier as a model.
func ProjectSeatQuota(o seatquota.Observation) (SeatQuota, error) {
	if !reportProvider(o.Provider) || o.ObservedAt.IsZero() {
		return SeatQuota{}, ErrProtocol
	}
	window := o.Window
	if window == "" {
		window = "unknown"
	}
	p := SeatQuota{
		Quota:   Quota{Window: window, UsedPercent: o.UsedPercent, ResetsAt: o.ResetsAt},
		SeatRef: o.SeatRef, WindowMinutes: o.WindowMinutes, Scope: o.Scope, Model: o.Model,
		Reached: o.Reached, Reason: o.Reason, Overage: o.Overage, ResetCreditsAvailable: o.ResetCreditsAvailable,
		Source: o.Source, Confidence: o.Confidence, AdapterVersion: o.AdapterVersion, ObservedAt: o.ObservedAt.UTC(),
	}
	if _, err := contract(true); err != nil {
		return SeatQuota{}, ErrProtocol
	}
	var out SeatQuota
	if err := copyReport(schemas.quota, p, &out); err != nil {
		return SeatQuota{}, err
	}
	return out, nil
}

// ProjectSeatUsage does not accept a requested-model fallback. An absent actual
// model stays absent; token consumption is never converted into quota capacity.
func ProjectSeatUsage(u seatquota.Usage) (Usage, error) {
	checked, err := seatquota.Consumption(u.SeatRef, u.Provider, u.ModelActual, u.InputTokens, u.OutputTokens, u.ObservedAt)
	if err != nil {
		return Usage{}, ErrProtocol
	}
	p := Usage{SeatRef: checked.SeatRef, ModelActual: checked.ModelActual, InputTokens: checked.InputTokens, OutputTokens: checked.OutputTokens}
	if _, err := contract(true); err != nil {
		return Usage{}, ErrProtocol
	}
	var out Usage
	if err := copyReport(schemas.usage, p, &out); err != nil {
		return Usage{}, err
	}
	return out, nil
}

func reportProvider(provider string) bool {
	switch provider {
	case "codex", "claude", "copilot", "muse":
		return true
	}
	return false
}

// The round trip validates the closed payload and owns all returned pointers.
func copyReport(schema *jsonschema.Schema, value, target any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > MaxEnvelopeBytes {
		return ErrProtocol
	}
	var payload any
	if json.Unmarshal(raw, &payload) != nil || schema.Validate(payload) != nil || json.Unmarshal(raw, target) != nil {
		return ErrProtocol
	}
	return nil
}
