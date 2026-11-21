//go:build linux

package containerjobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerExecJournalSurvivesRestartAndContainsNoRawIdentity(t *testing.T) {
	n := nativeProvisionFixture(t)
	if err := n.ClaimExecution(context.Background(), testOrg, "original/job-id"); err != nil {
		t.Fatal("first attempt refused", err)
	}
	names, err := os.ReadDir(filepath.Join(n.root, "exec-attempts"))
	if err != nil || len(names) != 1 || !hashPattern.MatchString(names[0].Name()) {
		t.Fatal("journal name is not a fixed hash")
	}
	data, err := os.ReadFile(filepath.Join(n.root, "exec-attempts", names[0].Name()))
	if err != nil || string(data) != "attempt-v1\n" {
		t.Fatal("journal retained source identity")
	}
	info, err := names[0].Info()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("attempt record is not private")
	}
	restarted := NewNativeWithPolicy(n.root, n.policy)
	restarted.openHostRoot = n.openHostRoot
	if restarted.ClaimExecution(context.Background(), testOrg, "original/job-id") == nil {
		t.Fatal("process restart forgot the spent execution")
	}
	if restarted.ClaimExecution(context.Background(), testOrg, "different-job") != nil {
		t.Fatal("new original delivery refused")
	}
	unconfigured := NewNative(n.root, nil)
	unconfigured.openHostRoot = n.openHostRoot
	if unconfigured.ClaimExecution(context.Background(), testOrg, "third-job") == nil {
		t.Fatal("missing local policy granted execution")
	}
}
func TestContainerExecUncertainJournalSyncNeverRefundsTheID(t *testing.T) {
	for stage := 1; stage <= 3; stage++ {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			n := nativeProvisionFixture(t)
			calls := 0
			n.syncJournal = func(f *os.File) error {
				calls++
				if calls == stage {
					return ErrUnavailable
				}
				return f.Sync()
			}
			if n.ClaimExecution(context.Background(), testOrg, "uncertain-job") == nil {
				t.Fatal("uncertain durable record granted execution")
			}
			n.syncJournal = nil
			if n.ClaimExecution(context.Background(), testOrg, "uncertain-job") == nil {
				t.Fatal("uncertain record was refunded")
			}
		})
	}
}
func TestContainerExecJournalRefusesFullOrMalformedStore(t *testing.T) {
	n := nativeProvisionFixture(t)
	dir := filepath.Join(n.root, "exec-attempts")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture")
	}
	for i := 0; i < maxHostEntries; i++ {
		if os.WriteFile(filepath.Join(dir, fmt.Sprintf("%064x", i)), nil, 0600) != nil {
			t.Fatal("fixture")
		}
	}
	if n.ClaimExecution(context.Background(), testOrg, "new-job") == nil {
		t.Fatal("full journal admitted an execution")
	}
	n = nativeProvisionFixture(t)
	dir = filepath.Join(n.root, "exec-attempts")
	if os.Mkdir(dir, 0700) != nil || os.WriteFile(filepath.Join(dir, "unknown-record"), nil, 0600) != nil {
		t.Fatal("fixture")
	}
	if n.ClaimExecution(context.Background(), testOrg, "new-job") == nil {
		t.Fatal("uncertain journal contents were ignored")
	}
	for _, jobID := range []string{"", strings.Repeat("x", 257), "nul\x00id"} {
		if n.ClaimExecution(context.Background(), testOrg, jobID) == nil {
			t.Fatal("invalid delivery identity admitted")
		}
	}
}
