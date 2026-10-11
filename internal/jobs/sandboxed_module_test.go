package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/services"
)

func okSpec() services.SandboxedModuleSpec {
	return services.SandboxedModuleSpec{Name: "extract", Image: "ghcr.io/aceteam-ai/extract-service:latest", ContainerPort: 8100, NetworkIndex: 0}
}

// fakeDeps builds sandboxRuntimeDeps whose behavior the test controls, so the
// probe/guard are exercised without a real container runtime.
func fakeDeps(preflightErr, engineErr error, imagePresent bool) sandboxRuntimeDeps {
	return sandboxRuntimeDeps{
		selectRuntime: func() catalog.ContainerRuntime { return catalog.ContainerRuntime{EngineBin: "docker"} },
		preflight:     func(catalog.ContainerRuntime) error { return preflightErr },
		engineReady:   func(context.Context, catalog.ContainerRuntime) error { return engineErr },
		imagePresent:  func(context.Context, catalog.ContainerRuntime, string) bool { return imagePresent },
	}
}

func TestSandboxedModuleCapabilitiesPreset(t *testing.T) {
	c := SandboxedModuleCapabilities()
	if !c.CooperativeCancel {
		t.Error("sandboxed module preset must request cooperative cancel")
	}
	if c.Lane != LaneHeavy {
		t.Errorf("Lane = %v, want LaneHeavy", c.Lane)
	}
	if c.Deadline != DeadlineLong {
		t.Errorf("Deadline = %v, want DeadlineLong", c.Deadline)
	}
	// The generic preset must NOT presume a metering posture: a consumer layers
	// its own WorkActions/ReceiptOwner. A non-empty default here would make the
	// runner fail a keyless node closed for every sandboxed job (the #1287
	// fail-closed scope must stay opt-in).
	if len(c.WorkActions) != 0 {
		t.Errorf("WorkActions = %v, want empty (consumer decides metering)", c.WorkActions)
	}
	if c.ReceiptOwner != ReceiptOwnerNone {
		t.Errorf("ReceiptOwner = %v, want ReceiptOwnerNone", c.ReceiptOwner)
	}
}

func TestProbeSandboxedRuntime(t *testing.T) {
	ctx := context.Background()

	t.Run("preflight fails -> unavailable", func(t *testing.T) {
		st := probeSandboxedRuntime(ctx, fakeDeps(errors.New("cpu controller not delegated"), nil, true))
		if st.Available {
			t.Fatal("expected unavailable when cgroup limits cannot be enforced")
		}
		if st.Reason == "" {
			t.Error("unavailable status must carry a reason")
		}
	})

	t.Run("engine unreachable -> unavailable", func(t *testing.T) {
		st := probeSandboxedRuntime(ctx, fakeDeps(nil, errors.New("cannot connect to docker daemon"), true))
		if st.Available {
			t.Fatal("expected unavailable when runtime is unreachable")
		}
	})

	t.Run("runtime ready -> available", func(t *testing.T) {
		st := probeSandboxedRuntime(ctx, fakeDeps(nil, nil, true))
		if !st.Available {
			t.Fatalf("expected available; reason=%q", st.Reason)
		}
		if st.Reason != "" {
			t.Errorf("available status must have no reason, got %q", st.Reason)
		}
	})
}

func TestEnsureSandboxedModuleAvailable(t *testing.T) {
	ctx := context.Background()

	t.Run("runtime unavailable -> typed terminal error", func(t *testing.T) {
		err := ensureSandboxedModuleAvailable(ctx, okSpec(), fakeDeps(errors.New("x"), nil, true))
		if !errors.Is(err, ErrSandboxedModuleUnavailable) {
			t.Fatalf("err = %v, want ErrSandboxedModuleUnavailable", err)
		}
	})

	t.Run("image missing (cannot pull, no network) -> typed terminal error", func(t *testing.T) {
		err := ensureSandboxedModuleAvailable(ctx, okSpec(), fakeDeps(nil, nil, false))
		if !errors.Is(err, ErrSandboxedModuleUnavailable) {
			t.Fatalf("err = %v, want ErrSandboxedModuleUnavailable", err)
		}
	})

	t.Run("runtime ready + image present -> nil", func(t *testing.T) {
		if err := ensureSandboxedModuleAvailable(ctx, okSpec(), fakeDeps(nil, nil, true)); err != nil {
			t.Fatalf("expected available, got %v", err)
		}
	})

	t.Run("invalid spec -> validation error (not the availability sentinel)", func(t *testing.T) {
		bad := okSpec()
		bad.Image = "alpine:latest" // not registry-qualified
		err := ensureSandboxedModuleAvailable(ctx, bad, fakeDeps(nil, nil, true))
		if err == nil {
			t.Fatal("expected a validation error for an unqualified image")
		}
		if errors.Is(err, ErrSandboxedModuleUnavailable) {
			t.Error("a spec validation failure must not masquerade as the availability sentinel")
		}
	})
}

func TestErrSandboxedModuleUnavailableJSONShape(t *testing.T) {
	var body struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(ErrSandboxedModuleUnavailable.Error()), &body); err != nil {
		t.Fatalf("sentinel message is not JSON: %v", err)
	}
	if body.Reason != "sandboxed_module_unavailable" {
		t.Errorf("reason = %q, want sandboxed_module_unavailable", body.Reason)
	}
	if body.Message == "" {
		t.Error("sentinel must carry an actionable message")
	}
}
