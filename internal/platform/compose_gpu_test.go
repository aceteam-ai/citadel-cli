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
					return nil, os.ErrNotExist
				}
				return nil, tc.devErr
			}, func(string) ([]byte, error) { return nil, os.ErrNotExist })
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
			}, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, func(string) ([]byte, error) { return nil, os.ErrNotExist })
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

// The PCI inventory intentionally contains only an Intel controller. Each Tegra
// observation must independently prevent it from proving NVIDIA absence.
func TestComposeGPUTegraEvidenceIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, marker, compatible string
		markerErr, readErr       error
		want                     int
	}{
		{name: "release marker", marker: "/etc/nv_tegra_release", want: -1},
		{name: "integrated GPU device", marker: "/dev/nvhost-ctrl-gpu", want: -1},
		{name: "device tree", compatible: "nvidia,p3701-0000\x00nvidia,tegra234\x00", want: -1},
		{name: "unreadable release marker", marker: "/etc/nv_tegra_release", markerErr: os.ErrPermission, want: -1},
		{name: "unreadable integrated device", marker: "/dev/nvhost-ctrl-gpu", markerErr: os.ErrPermission, want: -1},
		{name: "unreadable device tree", readErr: os.ErrPermission, want: -1},
		{name: "other integrated platform", compatible: "vendor,other-soc\x00", want: 0},
		{name: "all Tegra evidence absent", readErr: os.ErrNotExist, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pciCalls := 0
			got := composeGPUCount(context.Background(), func(_ context.Context, name string, _ ...string) ([]byte, error) {
				if name == "nvidia-smi" {
					return nil, os.ErrNotExist
				}
				if name != "lspci" {
					t.Fatalf("unexpected command %q", name)
				}
				pciCalls++
				return []byte("00: VGA compatible controller: Intel"), nil
			}, func(path string) (os.FileInfo, error) {
				if path == tc.marker {
					return nil, tc.markerErr
				}
				return nil, os.ErrNotExist
			}, func(path string) ([]byte, error) {
				if path != "/proc/device-tree/compatible" {
					t.Fatalf("unexpected read %q", path)
				}
				return []byte(tc.compatible), tc.readErr
			})
			if got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
			if tc.want == -1 && pciCalls != 0 {
				t.Fatal("Tegra uncertainty reached PCI verdict")
			}
			if tc.want == 0 && pciCalls != 1 {
				t.Fatal("absence skipped PCI proof")
			}
		})
	}
}

func TestComposeGPUCancellationDuringTegraReadIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := composeGPUCount(ctx, func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name != "nvidia-smi" {
			t.Fatal("cancelled observation reached PCI")
		}
		return nil, os.ErrNotExist
	}, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, func(string) ([]byte, error) {
		cancel()
		return nil, os.ErrNotExist
	})
	if got != -1 {
		t.Fatalf("cancelled observation returned %d", got)
	}
}
