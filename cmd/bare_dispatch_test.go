package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

func resetBareDispatchState(t *testing.T) {
	t.Helper()
	origStdoutTTY := bareStdoutIsTTYFn
	origStdinTTY := bareStdinIsTTYFn
	origState := bareHasMeshStateFn
	origCreds := bareHasDeviceCredentialsFn
	origResolve := bareResolveControlURLFn
	origEnrollCommand := bareRunEnrollCommandFn
	origEnroll := bareRunGuidedEnrollFn
	origSession := bareRunSessionDispatchStubFn
	t.Cleanup(func() {
		bareStdoutIsTTYFn = origStdoutTTY
		bareStdinIsTTYFn = origStdinTTY
		bareHasMeshStateFn = origState
		bareHasDeviceCredentialsFn = origCreds
		bareResolveControlURLFn = origResolve
		bareRunEnrollCommandFn = origEnrollCommand
		bareRunGuidedEnrollFn = origEnroll
		bareRunSessionDispatchStubFn = origSession
	})
}

func TestRunExistingGuidedEnrollReusesEnrollCommandThenSessionSeam(t *testing.T) {
	resetBareDispatchState(t)
	calledEnroll := 0
	bareRunEnrollCommandFn = func() { calledEnroll++ }
	bareHasMeshStateFn = func() bool { return true }
	bareHasDeviceCredentialsFn = func() bool { return true }
	bareResolveControlURLFn = func() string { return "https://self-hosted.example" }
	var gotTier enrollmentTier
	var gotURL string
	bareRunSessionDispatchStubFn = func(tier enrollmentTier, url string) {
		gotTier, gotURL = tier, url
	}

	runExistingGuidedEnroll()
	if calledEnroll != 1 {
		t.Fatalf("existing enroll command called %d times, want 1", calledEnroll)
	}
	if gotTier != enrollmentPlatform || gotURL != "https://self-hosted.example" {
		t.Fatalf("post-enroll session dispatch = (%q, %q), want (%q, persisted URL)", gotTier, gotURL, enrollmentPlatform)
	}
}

func TestEnrollmentTierFor(t *testing.T) {
	cases := []struct {
		name           string
		hasMeshState   bool
		hasDeviceCreds bool
		want           enrollmentTier
	}{
		{name: "no state is unenrolled", want: enrollmentUnenrolled},
		{name: "mesh state without device credentials", hasMeshState: true, want: enrollmentMesh},
		{name: "platform node", hasMeshState: true, hasDeviceCreds: true, want: enrollmentPlatform},
		{name: "stale credentials without mesh state are not enrolled", hasDeviceCreds: true, want: enrollmentUnenrolled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := enrollmentTierFor(tc.hasMeshState, tc.hasDeviceCreds); got != tc.want {
				t.Fatalf("enrollmentTierFor(%v, %v) = %q, want %q", tc.hasMeshState, tc.hasDeviceCreds, got, tc.want)
			}
		})
	}
}

func TestBareCommandActionFor(t *testing.T) {
	cases := []struct {
		name  string
		isTTY bool
		tier  enrollmentTier
		want  bareCommandAction
	}{
		{name: "non tty never starts anything", isTTY: false, tier: enrollmentUnenrolled, want: bareCommandStatus},
		{name: "non tty observes platform node", isTTY: false, tier: enrollmentPlatform, want: bareCommandStatus},
		{name: "tty unenrolled uses guided flow", isTTY: true, tier: enrollmentUnenrolled, want: bareCommandGuidedEnroll},
		{name: "tty mesh node uses session seam", isTTY: true, tier: enrollmentMesh, want: bareCommandSession},
		{name: "tty platform node uses session seam", isTTY: true, tier: enrollmentPlatform, want: bareCommandSession},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bareCommandActionFor(tc.isTTY, tc.tier); got != tc.want {
				t.Fatalf("bareCommandActionFor(%v, %q) = %q, want %q", tc.isTTY, tc.tier, got, tc.want)
			}
		})
	}
}

func TestRunBareCitadelNonTTYIsObservational(t *testing.T) {
	resetBareDispatchState(t)
	bareStdoutIsTTYFn = func() bool { return false }
	bareStdinIsTTYFn = func() bool { return false }
	bareHasMeshStateFn = func() bool { return true }
	bareHasDeviceCredentialsFn = func() bool { return true }
	bareRunGuidedEnrollFn = func() { t.Fatal("non-TTY bare command must not start enrollment") }
	bareRunSessionDispatchStubFn = func(enrollmentTier, string) {
		t.Fatal("non-TTY bare command must not start or attach a session")
	}

	var out bytes.Buffer
	cmd := &cobra.Command{Use: "citadel"}
	cmd.SetOut(&out)
	runBareCitadel(cmd, nil)
	if got, want := out.String(), "Citadel enrollment: platform-enrolled\n"; got != want {
		t.Fatalf("non-TTY output = %q, want %q", got, want)
	}
}

func TestRunBareCitadelUnenrolledUsesExistingGuidedFlow(t *testing.T) {
	resetBareDispatchState(t)
	bareStdoutIsTTYFn = func() bool { return true }
	bareStdinIsTTYFn = func() bool { return true }
	bareHasMeshStateFn = func() bool { return false }
	bareHasDeviceCredentialsFn = func() bool { return false }
	called := 0
	bareRunGuidedEnrollFn = func() { called++ }
	bareRunSessionDispatchStubFn = func(enrollmentTier, string) {
		t.Fatal("unenrolled node must not enter the session path")
	}

	runBareCitadel(&cobra.Command{Use: "citadel"}, nil)
	if called != 1 {
		t.Fatalf("guided enroll called %d times, want 1", called)
	}
}

func TestRunBareCitadelEnrolledUsesSessionSeamAndPersistedControlURL(t *testing.T) {
	resetBareDispatchState(t)
	bareStdoutIsTTYFn = func() bool { return true }
	bareStdinIsTTYFn = func() bool { return true }
	bareHasMeshStateFn = func() bool { return true }
	bareHasDeviceCredentialsFn = func() bool { return false }
	bareResolveControlURLFn = func() string { return "https://self-hosted.example" }
	bareRunGuidedEnrollFn = func() { t.Fatal("enrolled node must not restart enrollment") }
	var gotTier enrollmentTier
	var gotURL string
	bareRunSessionDispatchStubFn = func(tier enrollmentTier, controlURL string) {
		gotTier, gotURL = tier, controlURL
	}

	runBareCitadel(&cobra.Command{Use: "citadel"}, nil)
	if gotTier != enrollmentMesh {
		t.Fatalf("session tier = %q, want %q", gotTier, enrollmentMesh)
	}
	if gotURL != "https://self-hosted.example" {
		t.Fatalf("session control URL = %q, want persisted self-hosted URL", gotURL)
	}
}

func TestRunBareCitadelPipedStdinIsNonInteractive(t *testing.T) {
	resetBareDispatchState(t)
	bareStdoutIsTTYFn = func() bool { return true }
	bareStdinIsTTYFn = func() bool { return false }
	bareHasMeshStateFn = func() bool { return false }
	bareHasDeviceCredentialsFn = func() bool { return false }
	bareRunGuidedEnrollFn = func() { t.Fatal("piped stdin must not start device authorization") }
	bareRunSessionDispatchStubFn = func(enrollmentTier, string) {
		t.Fatal("piped stdin must not start or attach a session")
	}

	var out bytes.Buffer
	cmd := &cobra.Command{Use: "citadel"}
	cmd.SetOut(&out)
	runBareCitadel(cmd, nil)
	if got, want := out.String(), "Citadel enrollment: unenrolled\n"; got != want {
		t.Fatalf("piped-stdin output = %q, want %q", got, want)
	}
}
