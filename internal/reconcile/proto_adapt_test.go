package reconcile

import (
	"testing"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
)

func TestAdaptDesiredStateRunningAndStopped(t *testing.T) {
	pb := &fabricpb.DesiredState{
		Revision: "rev-7",
		Modules: []*fabricpb.DesiredModule{
			{
				Source:          "owner/repo@^1.2",
				Config:          map[string]string{"PORT": "8080"},
				DesiredStatus:   fabricpb.ModuleStatus_MODULE_STATUS_RUNNING,
				AllowPrivileged: true,
			},
			{
				Source:        "embedding",
				DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_STOPPED,
			},
		},
	}

	ds := adaptDesiredState(pb)

	if ds.Revision != "rev-7" {
		t.Fatalf("revision not carried: got %q", ds.Revision)
	}
	if len(ds.Modules) != 2 {
		t.Fatalf("want 2 modules, got %d", len(ds.Modules))
	}

	// Source must pass through UNCHANGED (requested-ref equality contract).
	m0 := ds.Modules[0]
	if m0.Source != "owner/repo@^1.2" {
		t.Errorf("source rewritten: got %q", m0.Source)
	}
	if m0.EffectiveStatus() != StatusRunning {
		t.Errorf("want running, got %q", m0.EffectiveStatus())
	}
	if m0.Config["PORT"] != "8080" {
		t.Errorf("config not carried: %v", m0.Config)
	}
	if !m0.AllowPrivileged {
		t.Errorf("allow_privileged not carried")
	}

	m1 := ds.Modules[1]
	if m1.EffectiveStatus() != StatusStopped {
		t.Errorf("want stopped, got %q", m1.EffectiveStatus())
	}
}

func TestAdaptDesiredModuleAbsentIsDropped(t *testing.T) {
	// ABSENT is realized by OMITTING the module from the authoritative desired set
	// (the engine then uninstalls it if installed).
	_, ok := adaptDesiredModule(&fabricpb.DesiredModule{
		Source:        "owner/repo@v1",
		DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_ABSENT,
	})
	if ok {
		t.Fatal("ABSENT module must be dropped from desired set")
	}
}

func TestAdaptDesiredModuleUnspecifiedIsRejected(t *testing.T) {
	_, ok := adaptDesiredModule(&fabricpb.DesiredModule{
		Source:        "owner/repo@v1",
		DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED,
	})
	if ok {
		t.Fatal("UNSPECIFIED module must be rejected")
	}
}

func TestAdaptDesiredModuleEmptySourceIsRejected(t *testing.T) {
	_, ok := adaptDesiredModule(&fabricpb.DesiredModule{
		Source:        "   ",
		DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING,
	})
	if ok {
		t.Fatal("empty-source module must be rejected")
	}
}

func TestAdaptDesiredStateDropsInvalidKeepsValid(t *testing.T) {
	pb := &fabricpb.DesiredState{
		Revision: "rev-1",
		Modules: []*fabricpb.DesiredModule{
			{Source: "keep", DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING},
			{Source: "drop-absent", DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_ABSENT},
			{Source: "drop-unspec", DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED},
		},
	}
	ds := adaptDesiredState(pb)
	if len(ds.Modules) != 1 || ds.Modules[0].Source != "keep" {
		t.Fatalf("want only 'keep', got %+v", ds.Modules)
	}
}

func TestAdaptDesiredStateNil(t *testing.T) {
	ds := adaptDesiredState(nil)
	if ds.Revision != "" || len(ds.Modules) != 0 {
		t.Fatalf("nil pb must yield zero DesiredState, got %+v", ds)
	}
}

func TestHealthToProto(t *testing.T) {
	cases := []struct {
		in         ModuleHealth
		wantStatus fabricpb.ModuleStatus
		wantHealth fabricpb.ModuleHealth
	}{
		{HealthRunning, fabricpb.ModuleStatus_MODULE_STATUS_RUNNING, fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY},
		{HealthStopped, fabricpb.ModuleStatus_MODULE_STATUS_STOPPED, fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED},
		{HealthError, fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED, fabricpb.ModuleHealth_MODULE_HEALTH_ERROR},
		{HealthUnknown, fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED, fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED},
	}
	for _, c := range cases {
		gotStatus, gotHealth := healthToProto(c.in)
		if gotStatus != c.wantStatus || gotHealth != c.wantHealth {
			t.Errorf("healthToProto(%q) = (%v,%v), want (%v,%v)", c.in, gotStatus, gotHealth, c.wantStatus, c.wantHealth)
		}
	}
}
