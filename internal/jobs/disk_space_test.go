package jobs

import (
	"strings"
	"testing"
)

// TestPlanDiskPreflight pins the pure decision at the heart of #828's fix:
// proceed (nil error) when free space covers the estimate plus margin, fail
// closed (non-nil, descriptive error) when it doesn't. No disk/network I/O.
func TestPlanDiskPreflight(t *testing.T) {
	tests := []struct {
		name           string
		requiredBytes  int64
		availableBytes int64
		marginBytes    int64
		wantErr        bool
	}{
		{
			name:           "fits comfortably",
			requiredBytes:  10 << 30, // 10 GiB
			availableBytes: 100 << 30,
			marginBytes:    2 << 30,
			wantErr:        false,
		},
		{
			name:           "fits exactly at the margin boundary",
			requiredBytes:  10 << 30,
			availableBytes: 12 << 30,
			marginBytes:    2 << 30,
			wantErr:        false,
		},
		{
			name:           "one byte short of the margin boundary fails closed",
			requiredBytes:  10 << 30,
			availableBytes: 12<<30 - 1,
			marginBytes:    2 << 30,
			wantErr:        true,
		},
		{
			name:           "the 161GB LTX-Video incident shape: nowhere close to fitting",
			requiredBytes:  161 << 30,
			availableBytes: 50 << 30,
			marginBytes:    2 << 30,
			wantErr:        true,
		},
		{
			name:           "the fixed ~19GB diffusers-filtered pull fits",
			requiredBytes:  19 << 30,
			availableBytes: 50 << 30,
			marginBytes:    2 << 30,
			wantErr:        false,
		},
		{
			// requiredBytes==0 alone does not waive the safety margin -- zero
			// bytes free is still zero bytes free.
			name:           "zero required bytes still needs the margin covered",
			requiredBytes:  0,
			availableBytes: 0,
			marginBytes:    2 << 30,
			wantErr:        true,
		},
		{
			name:           "zero required and zero margin always fits",
			requiredBytes:  0,
			availableBytes: 0,
			marginBytes:    0,
			wantErr:        false,
		},
		{
			name:           "negative margin is treated as zero, not subtracted",
			requiredBytes:  10 << 30,
			availableBytes: 10 << 30,
			marginBytes:    -5 << 30,
			wantErr:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := planDiskPreflight("/cache/huggingface", tt.requiredBytes, tt.availableBytes, tt.marginBytes)
			if tt.wantErr && err == nil {
				t.Fatalf("planDiskPreflight() = nil error, want a blocking error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("planDiskPreflight() = %v, want nil (should fit)", err)
			}
			if tt.wantErr && err != nil {
				// Downloading nothing is the whole point -- the error must say so.
				if !containsAll(err.Error(), "insufficient disk space", "downloading nothing") {
					t.Errorf("error message %q missing expected fail-closed language", err.Error())
				}
			}
		})
	}
}

func TestPlanDiskPreflightOverflowGuard(t *testing.T) {
	// A pathological margin that would overflow int64 when added must not wrap
	// around to a small/negative "needed" that spuriously passes.
	const maxInt64 = int64(1<<63 - 1)
	err := planDiskPreflight("/cache", maxInt64-100, 10, maxInt64)
	if err == nil {
		t.Fatal("expected overflow-guarded planDiskPreflight to still fail closed on an enormous requirement")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:         "0 B",
		512:       "512 B",
		1024:      "1.0 KiB",
		1536:      "1.5 KiB",
		1 << 20:   "1.0 MiB",
		19 << 30:  "19.0 GiB",
		161 << 30: "161.0 GiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestNearestExistingDir(t *testing.T) {
	dir := t.TempDir()
	// A deep, non-existent path under an existing tempdir should walk back up
	// to the tempdir itself.
	got := nearestExistingDir(dir + "/does/not/exist/yet")
	if got != dir {
		t.Errorf("nearestExistingDir = %q, want %q", got, dir)
	}

	// An already-existing path is returned unchanged.
	if got := nearestExistingDir(dir); got != dir {
		t.Errorf("nearestExistingDir(existing) = %q, want %q", got, dir)
	}
}
