package localcobrowse

import (
	"errors"
	"strings"
	"testing"
)

func TestOwnerSecondInstanceGetsOwnerError(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireOwner(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	first.SetClient("Example Agent")

	_, err = AcquireOwner(dir)
	var oe *OwnerError
	if !errors.As(err, &oe) {
		t.Fatalf("second acquire: want OwnerError, got %v", err)
	}
	if !oe.Known || oe.Info.Client != "Example Agent" || !strings.Contains(err.Error(), "Example Agent") {
		t.Fatalf("owner error should name the owning client: %+v %v", oe, err)
	}

	first.Release()
	again, err := AcquireOwner(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again.Release()
}
