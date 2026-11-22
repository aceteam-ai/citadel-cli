package platform

import (
	"context"
	"errors"
	"os"
	"reflect"
)

// Legacy recovery reads before probing or waiting for registry exclusion. Its
// eventual mutation is conditional on that complete snapshot, not just the ID
// or generation. A newer lifecycle record must survive stale retention work.
// Like the registry gate, this is in-process exclusion, not a cross-process CAS
// or authority to stop a browser. The caller must separately fence effects.
// A nil next deletes; replacement returns the actual saved timestamp so later
// retention accounting can use the newly committed snapshot.
func mutateRecordIfCurrent(dir string, expected sessionRecord, next *sessionRecord) (sessionRecord, error) {
	fail := func() (sessionRecord, error) {
		return sessionRecord{}, errors.New("browser registry snapshot changed or unavailable")
	}
	if expected.Version != sessionRecordVersion || !sessionIDPattern.MatchString(expected.ID) ||
		(next != nil && (next.ID != expected.ID || next.Version != sessionRecordVersion)) {
		return fail()
	}
	release, err := acquireBrowserRegistryGate(context.Background(), dir)
	if err != nil {
		return fail()
	}
	defer release()
	current, err := loadRecord(dir, expected.ID)
	if err != nil || !reflect.DeepEqual(current, expected) {
		return fail()
	}
	if next == nil {
		if os.Remove(recordFile(dir, expected.ID)) != nil {
			return fail()
		}
		return sessionRecord{}, nil
	}
	if saveRecordUnderRegistryGate(dir, *next) != nil {
		return fail()
	}
	// saveRecord owns UpdatedAt. Return its real snapshot, rather than a
	// proposed value that can never match a subsequent conditional prune.
	saved, err := loadRecord(dir, expected.ID)
	if err != nil {
		return fail()
	}
	return saved, nil
}
