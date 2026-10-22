package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/tui/sessionview"
	"github.com/spf13/cobra"
)

func bareAbsentDependencies() bareSessionDependencies {
	return bareSessionDependencies{
		resolveStateDir: func() (string, error) { return "fixture-state", nil },
		request: func(context.Context, string) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: "private-transport-url", Err: os.ErrNotExist}
		},
		probeContention: func(string) (bool, error) { return false, nil },
		observePresence: func(string) (bool, error) { return false, nil },
		startup:         func(*cobra.Command, []string) {},
		dispatch:        func(*cobra.Command, bool, bool) {},
		runViewer:       func(context.Context, string, sessionview.Snapshot) error { return nil },
	}
}

func runBareNoOwnerFixture(t *testing.T, cmd *cobra.Command, present bool) {
	t.Helper()
	dependencies := bareAbsentDependencies()
	dependencies.observePresence = func(string) (bool, error) { return present, nil }
	dependencies.dispatch = dispatchBareCitadel
	if err := runBareSessionSelection(cmd, nil, dependencies); err != nil {
		t.Fatal(err)
	}
}

func TestBareExistingSessionAttachesBeforeEnrollmentOrStartup(t *testing.T) {
	resetBareDispatchState(t)
	bareHasMeshStateFn = func() bool { t.Fatal("attach re-read collapsing enrollment state"); return false }
	bareHasDeviceCredentialsFn = func() bool { t.Fatal("attach read device credentials"); return false }
	dependencies := bareAbsentDependencies()
	resolutions, requests, viewers := 0, 0, 0
	dependencies.resolveStateDir = func() (string, error) { resolutions++; return "fixed-state", nil }
	dependencies.request = func(_ context.Context, stateDir string) (*http.Response, error) {
		requests++
		if stateDir != "fixed-state" {
			t.Fatal("status destination was re-resolved")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(validSessionView))}, nil
	}
	dependencies.probeContention = func(string) (bool, error) { t.Fatal("valid session required lock authority"); return false, nil }
	dependencies.observePresence = func(string) (bool, error) { t.Fatal("valid session enumerated hidden state"); return false, nil }
	dependencies.startup = func(*cobra.Command, []string) { t.Fatal("attach invoked mutating startup") }
	dependencies.dispatch = func(*cobra.Command, bool, bool) { t.Fatal("attach invoked owner/enrollment dispatch") }
	dependencies.runViewer = func(_ context.Context, stateDir string, status sessionview.Snapshot) error {
		viewers++
		if stateDir != "fixed-state" || status.PID != 123 || !status.Connected {
			t.Fatal("viewer lost the fixed destination or validated baseline")
		}
		return nil
	}
	if err := runBareSessionSelection(&cobra.Command{Use: "citadel"}, nil, dependencies); err != nil || resolutions != 1 || requests != 1 || viewers != 1 {
		t.Fatal("existing-session selection did not remain read-only and single-target")
	}
}

func TestBareSelectionRefusesEveryUnknownWithoutStartup(t *testing.T) {
	for _, refusal := range []string{"resolver", "transport", "permission", "missing-response", "http-404", "redirect", "tun", "malformed", "held", "lock-error", "state-error", "response-and-absence"} {
		t.Run(refusal, func(t *testing.T) {
			dependencies := bareAbsentDependencies()
			dependencies.startup = func(*cobra.Command, []string) { t.Fatal("unknown discovery invoked startup") }
			dependencies.dispatch = func(*cobra.Command, bool, bool) { t.Fatal("unknown discovery invoked enrollment/owner") }
			dependencies.runViewer = func(context.Context, string, sessionview.Snapshot) error { t.Fatal("unknown discovery attached a viewer"); return nil }
			private := errors.New("private-path-or-credential")
			switch refusal {
			case "resolver":
				dependencies.resolveStateDir = func() (string, error) { return "", private }
			case "transport", "permission":
				cause := private
				if refusal == "permission" {
					cause = os.ErrPermission
				}
				dependencies.request = func(context.Context, string) (*http.Response, error) { return nil, cause }
			case "missing-response":
				dependencies.request = func(context.Context, string) (*http.Response, error) { return nil, nil }
			case "held":
				dependencies.probeContention = func(string) (bool, error) { return true, nil }
			case "lock-error":
				dependencies.probeContention = func(string) (bool, error) { return false, private }
			case "state-error":
				dependencies.observePresence = func(string) (bool, error) { return false, private }
			default:
				dependencies.request = func(context.Context, string) (*http.Response, error) {
					code, body := http.StatusOK, "private-path-or-credential"
					switch refusal {
					case "http-404":
						code = http.StatusNotFound
					case "redirect":
						code = http.StatusFound
					case "tun":
						body = `{"BackendState":"Running"}`
					case "response-and-absence":
						return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(body))}, os.ErrNotExist
					}
					return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
			}
			err := runBareSessionSelection(&cobra.Command{Use: "citadel"}, nil, dependencies)
			if err != errSessionViewUnavailable || strings.Contains(err.Error(), "private-path-or-credential") {
				t.Fatal("unknown selection did not produce only the fixed refusal")
			}
		})
	}
}

func TestBareNoOwnerUsesCheckedPresenceAndStartsLegacyOncePerInvocation(t *testing.T) {
	resetBareDispatchState(t)
	bareHasMeshStateFn = func() bool { t.Fatal("checked presence was replaced by false-on-error HasState"); return false }
	dependencies := bareAbsentDependencies()
	var sequence []string
	resolutions := 0
	dependencies.resolveStateDir = func() (string, error) { resolutions++; return "fixed-state", nil }
	dependencies.probeContention = func(stateDir string) (bool, error) {
		if stateDir != "fixed-state" {
			t.Fatal("lock probe changed target")
		}
		sequence = append(sequence, "unheld")
		return false, nil
	}
	dependencies.observePresence = func(stateDir string) (bool, error) {
		if stateDir != "fixed-state" {
			t.Fatal("presence observation changed target")
		}
		sequence = append(sequence, "present")
		return true, nil
	}
	dependencies.startup = func(*cobra.Command, []string) { sequence = append(sequence, "startup") }
	dependencies.dispatch = func(_ *cobra.Command, interactive, present bool) {
		if !interactive || !present {
			t.Fatal("legacy dispatch lost checked presence")
		}
		sequence = append(sequence, "dispatch")
	}
	command := &cobra.Command{Use: "citadel"}
	for invocation := 0; invocation < 2; invocation++ {
		if err := runBareSessionSelection(command, nil, dependencies); err != nil {
			t.Fatal(err)
		}
	}
	if resolutions != 2 || strings.Join(sequence, ",") != "unheld,present,startup,dispatch,unheld,present,startup,dispatch" {
		t.Fatal("reused command retained a discovery decision or duplicated startup")
	}
	dependencies.resolveStateDir = func() (string, error) { return "", errors.New("changed invocation") }
	if err := runBareSessionSelection(command, nil, dependencies); err != errSessionViewUnavailable || len(sequence) != 8 {
		t.Fatal("later unknown invocation inherited an earlier startup authorization")
	}
}

func TestBareStartupDeferralIsExactInteractiveRootOnly(t *testing.T) {
	resetBareDispatchState(t)
	root := &cobra.Command{Use: "citadel"}
	status := &cobra.Command{Use: "status"}
	root.AddCommand(status)
	for _, fixture := range []struct {
		command *cobra.Command
		args    []string
		stdout  bool
		stdin   bool
		want    bool
	}{
		{root, nil, true, true, true},
		{root, []string{"extra"}, true, true, false},
		{root, nil, false, true, false},
		{root, nil, true, false, false},
		{status, nil, true, true, false},
		{&cobra.Command{Use: "other"}, nil, true, true, false},
	} {
		bareStdoutIsTTYFn = func() bool { return fixture.stdout }
		bareStdinIsTTYFn = func() bool { return fixture.stdin }
		if deferBareStartup(fixture.command, fixture.args) != fixture.want {
			t.Fatal("startup deferral escaped exact bare interactive invocation")
		}
	}
}

func TestBareCancelledInvocationCannotEnterStartup(t *testing.T) {
	for _, cancelBeforeProbe := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		command := &cobra.Command{Use: "citadel"}
		command.SetContext(ctx)
		dependencies := bareAbsentDependencies()
		dependencies.startup = func(*cobra.Command, []string) { t.Fatal("cancelled invocation started mutating hooks") }
		dependencies.dispatch = func(*cobra.Command, bool, bool) { t.Fatal("cancelled invocation entered enrollment/owner") }
		if cancelBeforeProbe {
			cancel()
			dependencies.resolveStateDir = func() (string, error) { t.Fatal("cancelled invocation began discovery"); return "", nil }
		} else {
			dependencies.observePresence = func(string) (bool, error) { cancel(); return false, nil }
		}
		if err := runBareSessionSelection(command, nil, dependencies); err != errSessionViewUnavailable {
			t.Fatal("cancelled invocation lost its static refusal")
		}
		cancel()
	}
}

func TestBareCancellationStopsAtEachDiscoveryBoundary(t *testing.T) {
	for _, stage := range []string{"resolver", "status", "absent", "lock"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command := &cobra.Command{Use: "citadel"}
			command.SetContext(ctx)
			dependencies := bareAbsentDependencies()
			var sequence []string
			body := &sessionViewBody{Reader: strings.NewReader(validSessionView)}
			dependencies.resolveStateDir = func() (string, error) {
				sequence = append(sequence, "resolve")
				if stage == "resolver" {
					cancel()
				}
				return "fixed-state", nil
			}
			dependencies.request = func(context.Context, string) (*http.Response, error) {
				if stage == "resolver" {
					t.Fatal("cancelled resolver continued to status")
				}
				sequence = append(sequence, "request")
				if stage == "status" {
					cancel()
					return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
				}
				if stage == "absent" {
					cancel()
				}
				return nil, os.ErrNotExist
			}
			dependencies.probeContention = func(string) (bool, error) {
				if stage != "lock" {
					t.Fatal("cancelled status request continued to lock observation")
				}
				sequence = append(sequence, "probe")
				cancel()
				return false, nil
			}
			dependencies.observePresence = func(string) (bool, error) { t.Fatal("cancelled lock observation continued to presence"); return false, nil }
			dependencies.startup = func(*cobra.Command, []string) { t.Fatal("cancelled discovery entered mutating startup") }
			dependencies.dispatch = func(*cobra.Command, bool, bool) { t.Fatal("cancelled discovery entered legacy dispatch") }
			dependencies.runViewer = func(context.Context, string, sessionview.Snapshot) error { t.Fatal("cancelled status entered a terminal viewer"); return nil }
			if err := runBareSessionSelection(command, nil, dependencies); err != errSessionViewUnavailable {
				t.Fatal("cancelled discovery lost its static refusal")
			}
			want := "resolve,request"
			if stage == "resolver" {
				want = "resolve"
			} else if stage == "lock" {
				want = "resolve,request,probe"
			}
			if strings.Join(sequence, ",") != want || (stage == "status" && !body.closed) {
				t.Fatal("cancelled discovery crossed a stage boundary or leaked the HTTP body")
			}
		})
	}
}
