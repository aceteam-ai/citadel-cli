package agentquota

import "testing"

func harnessFixture(h Harness) (Policy, Snapshot) {
	p, s := fixture()
	p.Lanes[0].Harness = h
	p.Lanes[0].MaxTurns = harnessMaximum(h)
	return p, s
}

func spendTurns(t *testing.T, p Policy, s Snapshot, count int) Snapshot {
	t.Helper()
	for range count {
		ref := Reservation{LaneID: "a", ReservationID: s.NextID}
		s = reserve(t, p, s, 0, "a")
		s = stop(t, p, s, 0, ref)
		s = inactive(t, p, s, 0, ref)
	}
	return s
}

func TestApprovedHarnessDefaults(t *testing.T) {
	c := DefaultHarnessConfig()
	if c.Codex != (TurnLimits{48, 30 * 60 * 1000, 1}) || c.ClaudeCode != (TurnLimits{10, 30 * 60 * 1000, 1}) {
		t.Fatalf("approved defaults changed: %#v", c)
	}
	if WindowMillis != 24*60*60*1000 || MaxChargesPerLane != 49 {
		t.Fatal("rolling window or retained-history bound changed")
	}
	p, s := fixture()
	p.Lanes = []LanePolicy{}
	plan, err := PlanReserve(p, s, contextFor(s, 0), "a")
	reject(t, plan, err, HoldLane)
	p, s = fixture()
	p.Lanes[0].Enabled = false
	plan, err = PlanReserve(p, s, contextFor(s, 0), "a")
	reject(t, plan, err, HoldLane)
}

func TestHarnessQuotaAndExactRollingBoundary(t *testing.T) {
	for _, tc := range []struct {
		h   Harness
		cap int
	}{{HarnessCodex, 48}, {HarnessClaudeCode, 10}} {
		t.Run(string(tc.h), func(t *testing.T) {
			p, s := harnessFixture(tc.h)
			s = spendTurns(t, p, s, tc.cap)
			for _, now := range []uint64{0, WindowMillis - 1} {
				plan, err := PlanReserve(p, s, contextFor(s, now), "a")
				reject(t, plan, err, HoldQuota)
			}
			other := reserve(t, p, s, 0, "b")
			if len(other.Lanes[0].Charges) != tc.cap || len(other.Lanes[1].Charges) != 1 {
				t.Fatal("one lane's cap affected another lane or erased its debt")
			}
			aged := reserve(t, p, s, WindowMillis, "a")
			if len(aged.Lanes[0].Charges) != 1 || aged.Lanes[0].Charges[0].ReservedAt != WindowMillis {
				t.Fatal("exact rolling boundary did not prune aged inactive charges")
			}
		})
	}
}

func TestHarnessAndLaneLimitsBothApply(t *testing.T) {
	for _, h := range []Harness{HarnessCodex, HarnessClaudeCode} {
		for _, global := range []bool{false, true} {
			p, s := harnessFixture(h)
			if global {
				if h == HarnessCodex {
					p.Harnesses.Codex.MaxTurns = 2
				} else {
					p.Harnesses.ClaudeCode.MaxTurns = 2
				}
			} else {
				p.Lanes[0].MaxTurns = 2
			}
			s = spendTurns(t, p, s, 2)
			plan, err := PlanReserve(p, s, contextFor(s, 0), "a")
			reject(t, plan, err, HoldQuota)
		}
	}
}

func TestHarnessReductionRechecksAttemptWithoutRefund(t *testing.T) {
	p, s := harnessFixture(HarnessCodex)
	s = spendTurns(t, p, s, 2)
	ref := Reservation{LaneID: "a", ReservationID: s.NextID}
	s = reserve(t, p, s, 0, "a")
	p.Harnesses.Codex.MaxTurns = 2
	plan, err := PlanAttempt(p, s, contextFor(s, 0), ref)
	reject(t, plan, err, HoldQuota)
	s = stop(t, p, s, 0, ref)
	s = inactive(t, p, s, 0, ref)
	if len(s.Lanes[0].Charges) != 3 {
		t.Fatal("lowered cap refunded debt")
	}
	plan, err = PlanReserve(p, s, contextFor(s, 0), "a")
	reject(t, plan, err, HoldQuota)
}

func TestHarnessDurationTightensAndCannotExtendStoredBudget(t *testing.T) {
	for _, h := range []Harness{HarnessCodex, HarnessClaudeCode} {
		p, s := harnessFixture(h)
		limits := &p.Harnesses.Codex
		if h == HarnessClaudeCode {
			limits = &p.Harnesses.ClaudeCode
		}
		s = reserve(t, p, s, 0, "a")
		limits.MaxTurnMillis = 100
		s = attempt(t, p, s, 0, Reservation{LaneID: "a", ReservationID: 1})
		if s.Lanes[0].Charges[0].TurnLimitMillis != 100 {
			t.Fatal("attempt ignored tighter harness budget")
		}
		limits.MaxTurnMillis = MaxTurnMillis
		plan, err := PlanReserve(p, s, contextFor(s, 100), "a")
		reject(t, plan, err, HoldDeadline)
		p, s = harnessFixture(h)
		if h == HarnessCodex {
			p.Harnesses.Codex.MaxTurnMillis = 100
		} else {
			p.Harnesses.ClaudeCode.MaxTurnMillis = 100
		}
		s = reserve(t, p, s, 0, "a")
		p.Harnesses = DefaultHarnessConfig()
		s = attempt(t, p, s, 0, Reservation{LaneID: "a", ReservationID: 1})
		if s.Lanes[0].Charges[0].TurnLimitMillis != 100 {
			t.Fatal("cap increase extended reserved budget")
		}
	}
}

func TestHarnessChangeRetainsDebtAndPermitsExactCleanup(t *testing.T) {
	p, s := harnessFixture(HarnessCodex)
	s = spendTurns(t, p, s, 47)
	ref := Reservation{LaneID: "a", ReservationID: s.NextID}
	s = reserve(t, p, s, 0, "a")
	p.Lanes[0].Harness, p.Lanes[0].MaxTurns = HarnessClaudeCode, 10
	if err := Validate(p, s); err != nil {
		t.Fatal("historical Codex debt became invalid under Claude policy", err)
	}
	plan, err := PlanAttempt(p, s, contextFor(s, 0), ref)
	reject(t, plan, err, HoldQuota)
	s = stop(t, p, s, 0, ref)
	s = inactive(t, p, s, 0, ref)
	if len(s.Lanes[0].Charges) != 48 {
		t.Fatal("harness change erased debt")
	}
	plan, err = PlanReserve(p, s, contextFor(s, 0), "a")
	reject(t, plan, err, HoldQuota)
	_ = reserve(t, p, s, WindowMillis, "a")
}

func TestEveryHarnessRetainsOccupiedCustody(t *testing.T) {
	for _, h := range []Harness{HarnessCodex, HarnessClaudeCode} {
		p, s := harnessFixture(h)
		ref := Reservation{LaneID: "a", ReservationID: 1}
		s = reserve(t, p, s, 0, "a")
		check := func() {
			t.Helper()
			plan, err := PlanReserve(p, s, contextFor(s, 0), "a")
			reject(t, plan, err, HoldBusy)
		}
		check()
		s = attempt(t, p, s, 0, ref)
		check()
		plan, err := PlanAttemptUncertain(p, s, contextFor(s, 0), ref)
		s = accept(t, p, s, plan, err)
		check()
		s = stop(t, p, s, 0, ref)
		check()
		plan, err = PlanStopUncertain(p, s, contextFor(s, 0), ref)
		s = accept(t, p, s, plan, err)
		check()
	}
}

func TestInvalidHarnessConfigurationFailsClosed(t *testing.T) {
	cases := map[string]func(*Policy){
		"missing config":          func(p *Policy) { p.Harnesses = HarnessConfig{} },
		"missing harness":         func(p *Policy) { p.Lanes[0].Harness = "" },
		"unknown harness":         func(p *Policy) { p.Lanes[0].Harness = "other" },
		"case mismatch":           func(p *Policy) { p.Lanes[0].Harness = "Codex" },
		"alias":                   func(p *Policy) { p.Lanes[0].Harness = "claude" },
		"codex raised":            func(p *Policy) { p.Harnesses.Codex.MaxTurns = 49 },
		"claude raised":           func(p *Policy) { p.Harnesses.ClaudeCode.MaxTurns = 11 },
		"codex zero":              func(p *Policy) { p.Harnesses.Codex.MaxTurns = 0 },
		"claude zero":             func(p *Policy) { p.Harnesses.ClaudeCode.MaxTurns = 0 },
		"codex duration zero":     func(p *Policy) { p.Harnesses.Codex.MaxTurnMillis = 0 },
		"claude duration zero":    func(p *Policy) { p.Harnesses.ClaudeCode.MaxTurnMillis = 0 },
		"codex duration raised":   func(p *Policy) { p.Harnesses.Codex.MaxTurnMillis++ },
		"claude duration raised":  func(p *Policy) { p.Harnesses.ClaudeCode.MaxTurnMillis++ },
		"codex concurrency":       func(p *Policy) { p.Harnesses.Codex.MaxActiveTurns = 2 },
		"claude concurrency":      func(p *Policy) { p.Harnesses.ClaudeCode.MaxActiveTurns = 2 },
		"codex no custody bound":  func(p *Policy) { p.Harnesses.Codex.MaxActiveTurns = 0 },
		"claude no custody bound": func(p *Policy) { p.Harnesses.ClaudeCode.MaxActiveTurns = 0 },
		"codex lane raised":       func(p *Policy) { p.Lanes[0].Harness = HarnessCodex; p.Lanes[0].MaxTurns = 49 },
		"claude lane raised":      func(p *Policy) { p.Lanes[0].MaxTurns = 11 },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			p, s := fixture()
			edit(&p)
			plan, err := PlanReserve(p, s, contextFor(s, 0), "a")
			reject(t, plan, err, HoldPolicy)
		})
	}
}
