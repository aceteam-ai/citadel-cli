package status

import (
	"math"
	"testing"
)

// realTopLTwo is representative `top -l 2 -n 0` output on macOS. It carries two
// "CPU usage:" lines; the second (delta) is the accurate reading and must be
// the one parseTopCPUUsage returns.
const realTopLTwo = `Processes: 512 total, 2 running, 510 sleeping, 2401 threads
2026/09/15 10:00:00
Load Avg: 2.10, 2.05, 2.00
CPU usage: 20.00% user, 15.00% sys, 65.00% idle
SharedLibs: 400M resident, 60M data, 20M linkedit.
MemRegions: 120000 total, 3000M resident, 90M private, 1200M shared.
PhysMem: 15G used (2100M wired), 900M unused.
VM: 3400G vsize, 2300M framework vsize, 0(0) swapins, 0(0) swapouts.
Networks: packets: 1/1K in, 1/1K out.
Disks: 1/1M read, 1/1M written.

Processes: 512 total, 2 running, 510 sleeping, 2401 threads
2026/09/15 10:00:01
Load Avg: 2.10, 2.05, 2.00
CPU usage: 3.84% user, 6.41% sys, 89.74% idle
SharedLibs: 400M resident, 60M data, 20M linkedit.
MemRegions: 120000 total, 3000M resident, 90M private, 1200M shared.
PhysMem: 15G used (2100M wired), 900M unused.
VM: 3400G vsize, 2300M framework vsize, 0(0) swapins, 0(0) swapouts.
Networks: packets: 1/1K in, 1/1K out.
Disks: 1/1M read, 1/1M written.
`

func TestParseTopCPUUsage(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    float64
		wantErr bool
	}{
		{
			name:  "two samples returns the last (delta) reading",
			input: realTopLTwo,
			want:  100 - 89.74, // 10.26
		},
		{
			name:  "single sample line",
			input: "CPU usage: 12.50% user, 7.50% sys, 80.00% idle\n",
			want:  20.0,
		},
		{
			name:  "fully idle",
			input: "CPU usage: 0.00% user, 0.00% sys, 100.00% idle\n",
			want:  0.0,
		},
		{
			name:  "fully busy",
			input: "CPU usage: 60.00% user, 40.00% sys, 0.00% idle\n",
			want:  100.0,
		},
		{
			name:  "extra whitespace variant",
			input: "CPU usage:  1.20%  user,   2.30%  sys,  96.50%  idle",
			want:  100 - 96.50, // 3.5
		},
		{
			name:    "no cpu usage line",
			input:   "Processes: 512 total\nLoad Avg: 1.0, 1.0, 1.0\n",
			wantErr: true,
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTopCPUUsage(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (value %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("parseTopCPUUsage() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseTopCPUUsage_InRange guards the clamp: any well-formed idle value
// yields a busy percentage within [0,100].
func TestParseTopCPUUsage_InRange(t *testing.T) {
	got, err := parseTopCPUUsage("CPU usage: 0.00% user, 0.00% sys, 99.99% idle")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got < 0 || got > 100 {
		t.Fatalf("busy %v out of range [0,100]", got)
	}
}
