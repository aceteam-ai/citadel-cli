package platform

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestComposeGPUObservationRequiresPositiveAbsence(t *testing.T) {
	for _, tc := range []struct {
		name, query, pci         string
		queryErr, pciErr, devErr error
		want                     int
	}{
		{name: "GPU count", query: "GPU A\nGPU B\n", want: 2},
		{name: "proven empty", pci: "00: VGA compatible controller: Intel", devErr: os.ErrNotExist, want: 0},
		{name: "missing utility with absent hardware", queryErr: os.ErrNotExist, pci: "00: VGA compatible controller: Intel", devErr: os.ErrNotExist, want: 0},
		{name: "failed driver with hardware", queryErr: errors.New("driver mismatch"), pci: "00: 3D controller: NVIDIA", devErr: os.ErrNotExist, want: -1},
		{name: "zero with hardware", pci: "00: VGA compatible controller: NVIDIA", devErr: os.ErrNotExist, want: -1},
		{name: "display hardware", pci: "00: Display controller: NVIDIA", devErr: os.ErrNotExist, want: -1},
		{name: "control device with negative inventory", pci: "00: VGA compatible controller: Intel", want: -1},
		{name: "device despite missing PCI tools", queryErr: os.ErrNotExist, pciErr: os.ErrNotExist, want: -1},
		{name: "unknown PCI", pciErr: os.ErrNotExist, devErr: os.ErrNotExist, want: -1},
		{name: "unreadable device", devErr: os.ErrPermission, want: -1},
		{name: "failed query output", query: "ambiguous", queryErr: errors.New("failure"), devErr: os.ErrNotExist, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := composeGPUCount(context.Background(), func(ctx context.Context, name string, args ...string) ([]byte, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 3*time.Second {
					t.Fatal("probe lacks shared bounded deadline")
				}
				if name == "nvidia-smi" {
					return []byte(tc.query), tc.queryErr
				}
				if name != "lspci" {
					t.Fatal("unexpected command", name)
				}
				return []byte(tc.pci), tc.pciErr
			}, func(path string) (os.FileInfo, error) {
				if path != "/dev/nvidiactl" {
					t.Fatal("unexpected hardware path", path)
				}
				return nil, tc.devErr
			})
			if got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestComposeGPUCancellationIsUnknownAndSkipsFurtherProbes(t *testing.T) {
	for _, stage := range []string{"query", "hardware"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			got := composeGPUCount(ctx, func(ctx context.Context, name string, args ...string) ([]byte, error) {
				calls++
				if stage == "query" || name == "lspci" {
					cancel()
					return nil, nil
				}
				return nil, nil
			}, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
			wantCalls := 1
			if stage == "hardware" {
				wantCalls = 2
			}
			if got != -1 || calls != wantCalls {
				t.Fatal("cancelled observation did not hold", got, calls)
			}
		})
	}
	if ComposeGPUProbeTimeout != 3*time.Second {
		t.Fatal("probe budget changed")
	}
}
