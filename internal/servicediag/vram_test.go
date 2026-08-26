package servicediag

import "testing"

func TestVRAMFitCheck(t *testing.T) {
	gb := func(n uint64) uint64 { return n * 1024 * 1024 * 1024 }

	tests := []struct {
		name        string
		freeBytes   uint64
		freeKnown   bool
		needMB      int
		needKnown   bool
		wantVerdict string
	}{
		{"fits", gb(16), true, 8000, true, VerdictOK},
		{"does_not_fit", gb(4), true, 22000, true, VerdictFail},
		{"need_unknown", gb(16), true, 0, false, VerdictUnknown},
		{"free_unknown", 0, false, 8000, true, VerdictUnknown},
		{"both_unknown", 0, false, 0, false, VerdictUnknown},
		{"exact_fit", gb(8), true, 8192, true, VerdictOK}, // 8GB free == 8192MB needed
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VRAMFitCheck(tt.freeBytes, tt.freeKnown, tt.needMB, tt.needKnown)
			if got.Verdict != tt.wantVerdict {
				t.Errorf("VRAMFitCheck() verdict = %q, want %q (detail: %s)", got.Verdict, tt.wantVerdict, got.Detail)
			}
			if got.Name != VRAMFitCheckName {
				t.Errorf("VRAMFitCheck() name = %q, want %q", got.Name, VRAMFitCheckName)
			}
			if got.Detail == "" {
				t.Error("VRAMFitCheck() Detail should never be empty")
			}
		})
	}
}
