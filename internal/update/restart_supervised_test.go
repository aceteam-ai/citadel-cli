package update

import "testing"

// TestSupervisedRestartIsAStaticFlag pins that the Windows restart decision
// comes from the supervisor flag alone, never from a probe that could fail and
// turn the restart into an unsupervised spawned copy.
func TestSupervisedRestartIsAStaticFlag(t *testing.T) {
	t.Cleanup(func() { SetSupervisedRestart(false) })
	if restartBySupervisor() {
		t.Fatal("supervised restart on by default")
	}
	SetSupervisedRestart(true)
	if !restartBySupervisor() {
		t.Fatal("supervised restart not honoured")
	}
	SetSupervisedRestart(false)
	if restartBySupervisor() {
		t.Fatal("supervised restart not cleared")
	}
}
