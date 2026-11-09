package localcobrowse

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestOwnerPermissionDeniedIsOwnerError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses a named mutex, not a lock file")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local-cobrowse.lock"), nil, 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := AcquireOwner(dir)
	var oe *OwnerError
	if !errors.As(err, &oe) {
		t.Fatalf("a lock file another account owns must read as an owner error, got %v", err)
	}
}
