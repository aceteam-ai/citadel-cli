package containerjobs

import (
	"context"
	"testing"
)

func TestContainerExecOriginalJobIDIsSpentBeforeAttemptAndNeverRefunded(t *testing.T) {
	f := newFake(t)
	e := &Engine{Runtime: f, EnrolledOrg: fixtureEnrolledOrg}
	r := baseRequest()
	r.Argv = []string{"fixture"}
	r.TimeoutSeconds = 30
	if _, err := e.Run(context.Background(), Exec, r); err == nil || len(f.effects) != 0 {
		t.Fatal("missing delivery identity executed")
	}
	if _, err := e.RunDelivery(context.Background(), Exec, r, "original-job"); err != nil {
		t.Fatal("original bounded execution refused", err)
	}
	if len(f.effects) != 1 || f.effects[0] != "exec" {
		t.Fatal("original execution missing")
	}
	if _, err := e.RunDelivery(context.Background(), Exec, r, "original-job"); err == nil || len(f.effects) != 1 {
		t.Fatal("redelivery repeated execution")
	}
	// Reusing the persistent runtime from another engine models process restart.
	e = &Engine{Runtime: f, EnrolledOrg: fixtureEnrolledOrg}
	if _, err := e.RunDelivery(context.Background(), Exec, r, "original-job"); err == nil || len(f.effects) != 1 {
		t.Fatal("restarted engine forgot the spent job")
	}
}
