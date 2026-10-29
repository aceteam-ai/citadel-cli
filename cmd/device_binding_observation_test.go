package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

func deviceBindingObservationFixture(t *testing.T) (*DeviceConfig, deviceLinkObservation, *int) {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		dir := filepath.Join(root, key)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	dc := &DeviceConfig{
		DeviceAPIToken: "fixture-current-secret", APIBaseURL: "https://api.invalid/tenant",
		OrgID: "11111111-2222-4333-8444-abcdefabcdef", NexusURL: "https://mesh.invalid",
	}
	calls := 0
	observation := deviceLinkObservation{
		config: func() *DeviceConfig { value := *dc; return &value },
		liveMesh: func(context.Context) deviceLinkMeshIdentity {
			return deviceLinkMeshIdentity{NodeID: "42", ControlURL: "https://mesh.invalid"}
		},
		controlURL: func() string { return "https://mesh.invalid" },
		defaultURL: func() string { return "https://default.invalid" },
		lookup: func(ctx context.Context, input nexus.DeviceBindingRequest) nexus.DeviceBindingState {
			calls++
			if input.Bearer != dc.DeviceAPIToken || input.OrgID != dc.OrgID || input.NodeID != "42" ||
				input.APIBaseURL != dc.APIBaseURL && !(dc.APIBaseURL == "" && input.APIBaseURL == "https://default.invalid") {
				t.Error("lookup did not capture the selected credential, origin, org and live identity")
			}
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > nexus.CurrentDeviceBindingTimeout {
				t.Error("display observation omitted its bounded monotonic deadline")
			}
			return nexus.BindingConfirmed
		},
	}
	return dc, observation, &calls
}

func TestDeviceBindingOnlyVerifiedObservationSuppressesWarning(t *testing.T) {
	for _, state := range []nexus.DeviceBindingState{
		nexus.BindingConfirmed, nexus.BindingUnknown, nexus.BindingNotLinked, nexus.BindingRefused, nexus.BindingUnsupported,
	} {
		dc, observation, calls := deviceBindingObservationFixture(t)
		lookup := observation.lookup
		observation.lookup = func(ctx context.Context, input nexus.DeviceBindingRequest) nexus.DeviceBindingState {
			lookup(ctx, input)
			return state
		}
		var output bytes.Buffer
		printDeviceLinkDiagnosticWith(context.Background(), &output, dc, observation)
		if (output.Len() == 0) != (state == nexus.BindingConfirmed) || *calls != 1 {
			t.Fatalf("state=%v warning=%t calls=%d", state, output.Len() != 0, *calls)
		}
		for _, private := range []string{dc.DeviceAPIToken, dc.OrgID, dc.APIBaseURL, dc.NexusURL} {
			if strings.Contains(output.String(), private) {
				t.Fatal("display exposed credential or identity configuration")
			}
		}
		if state == nexus.BindingRefused && (!strings.Contains(output.String(), "predates server verification") ||
			!strings.Contains(output.String(), "fresh browser approval") || !strings.Contains(output.String(), "permissions or membership")) {
			t.Fatal("old-key/refusal guidance lost D1 or claimed a specific 403 cause")
		}
	}
}

func TestDeviceBindingLateInputChangesCannotSuppressWarning(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*DeviceConfig, *deviceLinkObservation)
	}{
		{"bearer replaced", func(dc *DeviceConfig, _ *deviceLinkObservation) { dc.DeviceAPIToken = "replacement-secret" }},
		{"org changed", func(dc *DeviceConfig, _ *deviceLinkObservation) { dc.OrgID = "22222222-3333-4444-8555-aaaaaaaaaaaa" }},
		{"origin changed", func(dc *DeviceConfig, _ *deviceLinkObservation) { dc.APIBaseURL = "https://other.invalid/tenant" }},
		{"base path changed", func(dc *DeviceConfig, _ *deviceLinkObservation) { dc.APIBaseURL += "/other" }},
		{"control plane changed", func(dc *DeviceConfig, _ *deviceLinkObservation) { dc.NexusURL = "https://other-mesh.invalid" }},
		{"credential removed", func(_ *DeviceConfig, o *deviceLinkObservation) { o.config = func() *DeviceConfig { return nil } }},
		{"live mesh changed", func(_ *DeviceConfig, o *deviceLinkObservation) {
			o.liveMesh = func(context.Context) deviceLinkMeshIdentity {
				return deviceLinkMeshIdentity{NodeID: "43", ControlURL: "https://mesh.invalid"}
			}
		}},
		{"live mesh lost", func(_ *DeviceConfig, o *deviceLinkObservation) {
			o.liveMesh = func(context.Context) deviceLinkMeshIdentity { return deviceLinkMeshIdentity{} }
		}},
		{"live control plane changed with same ID", func(_ *DeviceConfig, o *deviceLinkObservation) {
			o.liveMesh = func(context.Context) deviceLinkMeshIdentity {
				return deviceLinkMeshIdentity{NodeID: "42", ControlURL: "https://other-mesh.invalid"}
			}
		}},
		{"resolved control plane changed", func(_ *DeviceConfig, o *deviceLinkObservation) {
			o.controlURL = func() string { return "https://other-mesh.invalid" }
		}},
		{"both control planes changed with same ID", func(_ *DeviceConfig, o *deviceLinkObservation) {
			o.controlURL = func() string { return "https://other-mesh.invalid" }
			o.liveMesh = func(context.Context) deviceLinkMeshIdentity {
				return deviceLinkMeshIdentity{NodeID: "42", ControlURL: "https://other-mesh.invalid"}
			}
		}},
		{"default origin changed", func(_ *DeviceConfig, o *deviceLinkObservation) {
			o.defaultURL = func() string { return "https://new-default.invalid" }
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			dc, observation, calls := deviceBindingObservationFixture(t)
			if change.name == "default origin changed" {
				dc.APIBaseURL = ""
			}
			initial := *dc
			lookup := observation.lookup
			observation.lookup = func(ctx context.Context, input nexus.DeviceBindingRequest) nexus.DeviceBindingState {
				result := lookup(ctx, input)
				change.edit(dc, &observation)
				return result
			}
			// The functions consult the mutable fixture source, while the
			// observer itself receives a stable copy of its dependencies.
			dynamic := observation
			dynamic.config = func() *DeviceConfig { return observation.config() }
			dynamic.liveMesh = func(ctx context.Context) deviceLinkMeshIdentity { return observation.liveMesh(ctx) }
			dynamic.controlURL = func() string { return observation.controlURL() }
			dynamic.defaultURL = func() string { return observation.defaultURL() }
			if detail := observeDeviceLinkDiagnostic(context.Background(), &initial, dynamic); detail == "" || *calls != 1 {
				t.Fatalf("late %s change retained confirmation or retried lookup", change.name)
			}
		})
	}
}

func TestDeviceBindingRequiresLiveAndResolvedControlPlaneAgreementBeforeHTTP(t *testing.T) {
	for _, test := range []struct {
		name, live, resolved string
		verified             bool
	}{
		{"matching", "https://mesh.invalid", "https://mesh.invalid", true},
		{"normalized", "  https://mesh.invalid/ ", "https://mesh.invalid", true},
		{"different namespace same numeric ID", "https://other-mesh.invalid", "https://mesh.invalid", false},
		{"missing live URL", "", "https://mesh.invalid", false},
		{"missing resolved URL", "https://mesh.invalid", "", false},
		{"invalid URLs", "not-a-url", "not-a-url", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dc, observation, calls := deviceBindingObservationFixture(t)
			observation.liveMesh = func(context.Context) deviceLinkMeshIdentity {
				return deviceLinkMeshIdentity{NodeID: "42", ControlURL: test.live}
			}
			observation.controlURL = func() string { return test.resolved }
			detail := observeDeviceLinkDiagnostic(context.Background(), dc, observation)
			wantCalls := 0
			if test.verified {
				wantCalls = 1
			}
			if (detail == "") != test.verified || *calls != wantCalls {
				t.Fatalf("control-plane check: verified=%t calls=%d want=%d", detail == "", *calls, wantCalls)
			}
		})
	}
}

func TestDeviceBindingDiagnosticAcceptsNilCommandContext(t *testing.T) {
	dc, observation, calls := deviceBindingObservationFixture(t)
	command := &cobra.Command{}
	if command.Context() != nil {
		t.Fatal("direct Run fixture unexpectedly has a context")
	}
	var output bytes.Buffer
	printDeviceLinkDiagnosticWith(command.Context(), &output, dc, observation)
	if output.Len() != 0 || *calls != 1 {
		t.Fatal("nil command context lost the bounded verified observation")
	}
}

func TestDeviceBindingDiagnosticUsesCancelledCommandContext(t *testing.T) {
	dc, observation, calls := deviceBindingObservationFixture(t)
	command := &cobra.Command{}
	ctx, cancel := context.WithCancel(context.Background())
	command.SetContext(ctx)
	cancel()
	var output bytes.Buffer
	printDeviceLinkDiagnosticWith(command.Context(), &output, dc, observation)
	if output.Len() == 0 || *calls != 0 {
		t.Fatal("cancelled command lost warning or started a lookup")
	}
}

func TestDeviceBindingMissingLiveMeshCannotBorrowSavedFabricIdentity(t *testing.T) {
	dc, observation, calls := deviceBindingObservationFixture(t)
	dc.FabricNodeID = "42"
	observation.liveMesh = func(context.Context) deviceLinkMeshIdentity { return deviceLinkMeshIdentity{} }
	if observeDeviceLinkDiagnostic(context.Background(), dc, observation) == "" || *calls != 0 {
		t.Fatal("saved fabric identity substituted for a missing live mesh identity")
	}
}

func TestDeviceBindingPreRequestChangeAndCancellationKeepWarning(t *testing.T) {
	for _, when := range []string{"before capture", "before request", "during request", "during input recheck"} {
		t.Run(when, func(t *testing.T) {
			dc, observation, calls := deviceBindingObservationFixture(t)
			initial := *dc
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch when {
			case "before capture":
				dc.DeviceAPIToken = "new-secret"
			case "before request":
				cancel()
			case "during request":
				lookup := observation.lookup
				observation.lookup = func(ctx context.Context, input nexus.DeviceBindingRequest) nexus.DeviceBindingState {
					result := lookup(ctx, input)
					cancel()
					return result
				}
			case "during input recheck":
				read := observation.config
				reads := 0
				observation.config = func() *DeviceConfig {
					reads++
					if reads == 2 {
						cancel()
					}
					return read()
				}
			}
			if observeDeviceLinkDiagnostic(ctx, &initial, observation) == "" {
				t.Fatal("changed or cancelled request suppressed the warning")
			}
			wantCalls := 1
			if when == "before capture" || when == "before request" {
				wantCalls = 0
			}
			if *calls != wantCalls {
				t.Fatalf("lookup calls=%d want=%d", *calls, wantCalls)
			}
		})
	}
}

func TestDeviceBindingConfirmationCannotSurviveAnotherInvocationOrReplacement(t *testing.T) {
	dc, observation, calls := deviceBindingObservationFixture(t)
	if observeDeviceLinkDiagnostic(context.Background(), dc, observation) != "" {
		t.Fatal("verified current-input positive control refused")
	}
	lookup := observation.lookup
	observation.lookup = func(ctx context.Context, input nexus.DeviceBindingRequest) nexus.DeviceBindingState {
		lookup(ctx, input)
		return nexus.BindingUnknown
	}
	if observeDeviceLinkDiagnostic(context.Background(), dc, observation) == "" || *calls != 2 {
		t.Fatal("earlier confirmation was reused on an unavailable invocation")
	}
	dc.DeviceAPIToken = "fresh-replacement-secret"
	observation.lookup = func(ctx context.Context, input nexus.DeviceBindingRequest) nexus.DeviceBindingState {
		lookup(ctx, input)
		return nexus.BindingNotLinked
	}
	if observeDeviceLinkDiagnostic(context.Background(), dc, observation) != deviceLinkNotLinked || *calls != 3 {
		t.Fatal("replacement inherited an earlier key's verified display")
	}
	// Refresh collectors remain offline and cannot reuse the initial render.
	if deviceLinkDiagnostic(dc) == "" || *calls != 3 {
		t.Fatal("offline refresh borrowed a previous observation or polled")
	}
}

func TestDeviceBindingObservationNeverWritesPreferredOrFallbackCredentials(t *testing.T) {
	dc, observation, _ := deviceBindingObservationFixture(t)
	preferred, legacy := t.TempDir(), t.TempDir()
	preferredBytes := []byte("device_api_token: fixture-current-secret\napi_base_url: https://api.invalid/tenant\norg_id: 11111111-2222-4333-8444-abcdefabcdef\nnexus_url: https://mesh.invalid\n")
	legacyBytes := []byte("device_api_token: legacy-secret\norg_id: legacy-org\n")
	for path, data := range map[string][]byte{filepath.Join(preferred, "config.yaml"): preferredBytes, filepath.Join(legacy, "config.yaml"): legacyBytes} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	observation.config = func() *DeviceConfig { return readDeviceConfigFromDirs([]string{preferred, legacy}) }
	if observeDeviceLinkDiagnostic(context.Background(), dc, observation) != "" {
		t.Fatal("read-only preferred credential control refused")
	}
	for path, want := range map[string][]byte{filepath.Join(preferred, "config.yaml"): preferredBytes, filepath.Join(legacy, "config.yaml"): legacyBytes} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("binding observation changed a credential file")
		}
	}
}
