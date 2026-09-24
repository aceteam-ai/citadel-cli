package cmd

import (
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

type recordingExclusiveStarter struct {
	calls                 int
	jobID, service, model string
	budget                uint64
	budgeted              bool
	reservation           *jobs.Reservation
	err                   error
}

func (s *recordingExclusiveStarter) StartExclusiveWithModel(_ jobs.JobContext, jobID, service, model string, budget uint64, budgeted bool) (*jobs.Reservation, []byte, error) {
	s.calls++
	s.jobID, s.service, s.model, s.budget, s.budgeted = jobID, service, model, budget, budgeted
	return s.reservation, nil, s.err
}

func TestCLIExclusiveUsesOneAtomicAdmissionAndPreservesRollbackResult(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gb         float64
		wantBudget uint64
		budgeted   bool
	}{
		{"whole card", 0, 0, false},
		{"bounded", 8, 8 * 1024 * 1024 * 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &recordingExclusiveStarter{reservation: &jobs.Reservation{JobID: "exclusive:target", Evicted: []string{"peer"}}, err: errors.New("rollback incomplete")}
			res, err := startExclusiveForCLI(s, jobs.JobContext{}, "exclusive:target", "target", tc.gb)
			if s.calls != 1 || s.jobID != "exclusive:target" || s.service != "target" || s.model != "" || s.budget != tc.wantBudget || s.budgeted != tc.budgeted {
				t.Fatalf("atomic admission routing: %+v", s)
			}
			if res != s.reservation || !errors.Is(err, s.err) {
				t.Fatalf("lost cleanup result: res=%v err=%v", res, err)
			}
		})
	}
}
