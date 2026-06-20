package platform

import (
	"testing"
)

func TestParseProxmoxNodeList(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNames []string
		wantErr   bool
	}{
		{
			name:      "single node",
			input:     `[{"node":"pve","status":"online","maxcpu":16,"maxmem":68719476736}]`,
			wantNames: []string{"pve"},
		},
		{
			name:      "cluster with three nodes",
			input:     `[{"node":"pve1","status":"online"},{"node":"pve2","status":"online"},{"node":"pve3","status":"offline"}]`,
			wantNames: []string{"pve1", "pve2", "pve3"},
		},
		{
			name:      "empty array",
			input:     `[]`,
			wantNames: nil,
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantErr: true,
		},
		{
			name:      "node with empty name is skipped",
			input:     `[{"node":"pve1"},{"node":""},{"node":"pve2"}]`,
			wantNames: []string{"pve1", "pve2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names, err := parseProxmoxNodeList([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(names) != len(tt.wantNames) {
				t.Fatalf("got %d names, want %d: %v", len(names), len(tt.wantNames), names)
			}
			for i, name := range names {
				if name != tt.wantNames[i] {
					t.Errorf("name[%d] = %q, want %q", i, name, tt.wantNames[i])
				}
			}
		})
	}
}

func TestParseProxmoxGuestCount(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{
			name:  "three VMs",
			input: `[{"vmid":100,"name":"vm1","status":"running"},{"vmid":101,"name":"vm2","status":"stopped"},{"vmid":102,"name":"vm3","status":"running"}]`,
			want:  3,
		},
		{
			name:  "no guests",
			input: `[]`,
			want:  0,
		},
		{
			name:  "single container",
			input: `[{"vmid":200,"name":"ct1","status":"running"}]`,
			want:  1,
		},
		{
			name:    "invalid json",
			input:   `{not valid}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count, err := parseProxmoxGuestCount([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if count != tt.want {
				t.Errorf("got count %d, want %d", count, tt.want)
			}
		})
	}
}

func TestDetectProxmox_NonProxmoxHost(t *testing.T) {
	// On a non-Proxmox system, DetectProxmox should return IsInstalled=false
	// without error. This test validates the fast-path early return.
	info, err := DetectProxmox()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil ProxmoxInfo")
	}
	if info.IsInstalled {
		// If this test is being run on an actual Proxmox host, skip instead of fail
		t.Skip("running on a Proxmox host; skipping non-Proxmox assertion")
	}
	if info.Version != "" {
		t.Errorf("expected empty version on non-Proxmox host, got %q", info.Version)
	}
	if info.NodeName != "" {
		t.Errorf("expected empty node name on non-Proxmox host, got %q", info.NodeName)
	}
}
