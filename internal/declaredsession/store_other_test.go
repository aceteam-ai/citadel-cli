//go:build !linux

package declaredsession

import (
	"errors"
	"testing"
)

func TestUnsupportedStoreDoesNotClaimDurability(t *testing.T) {
	if store, err := OpenStore(t.TempDir()); store != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported store accepted: %v", err)
	}
}
