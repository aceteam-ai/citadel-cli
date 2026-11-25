package compose

import (
	"reflect"
	"testing"
)

func TestLegacyRemovalExcludesReplacementBackup(t *testing.T) {
	out := []byte("old-id citadel-tei\nbackup-id citadel-tei-citadel-prev\n")
	got := legacyRemovalIDs(out, BackupContainerName("citadel-tei"))
	if !reflect.DeepEqual(got, []string{"old-id"}) {
		t.Fatalf("legacy cleanup selected %v; backup must remain recoverable", got)
	}
}
