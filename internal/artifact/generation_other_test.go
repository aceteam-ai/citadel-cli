//go:build !linux

package artifact

import (
	"os"
	"strings"
	"testing"
)

func TestUnsupportedPlatformRefusesGeneration(t *testing.T) {
	if Supported() || ValidGeneration(Algorithm+":"+strings.Repeat("a", 64)) {
		t.Fatal("advertised unsupported identity")
	}
	f, err := os.CreateTemp(t.TempDir(), "artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := Capture(f); err == nil {
		t.Fatal("issued unsupported identity")
	}
	if _, err := (Identity{}).Generation("sha256:" + strings.Repeat("a", 64)); err == nil {
		t.Fatal("issued unsupported generation")
	}
}
