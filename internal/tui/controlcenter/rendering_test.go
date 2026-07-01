package controlcenter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// TestApplyFullscreenRendering asserts the launch-time consumer stages the
// TCELL_ALTSCREEN flag that the vendored tcell reads at screen Init(): fullscreen
// on clears it (alternate screen used), fullscreen off sets "disable" (output to
// normal scrollback). This is the pure unit that "applies" the pref without a
// terminal or a running app.
func TestApplyFullscreenRendering(t *testing.T) {
	cases := []struct {
		name       string
		seed       string // value of TCELL_ALTSCREEN before the call
		fullscreen bool
		wantEnv    string // "" means the var must be unset
		wantReturn string
	}{
		{"fullscreen on unsets", "", true, "", ""},
		{"fullscreen off sets disable", "", false, "disable", "disable"},
		{"fullscreen on clears a stale disable", "disable", true, "", ""},
		{"fullscreen off is idempotent", "disable", false, "disable", "disable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv isolates and auto-restores the process env per subtest.
			t.Setenv(tcellAltScreenEnv, tc.seed)
			if tc.seed == "" {
				// t.Setenv cannot set an empty-but-present value meaningfully for
				// this test's "unset" seed, so clear it explicitly.
				_ = os.Unsetenv(tcellAltScreenEnv)
			}

			got := applyFullscreenRendering(tc.fullscreen)
			if got != tc.wantReturn {
				t.Errorf("applyFullscreenRendering(%v) return = %q, want %q",
					tc.fullscreen, got, tc.wantReturn)
			}

			env, present := os.LookupEnv(tcellAltScreenEnv)
			if tc.wantEnv == "" {
				if present {
					t.Errorf("TCELL_ALTSCREEN present = %q, want unset", env)
				}
				return
			}
			if !present || env != tc.wantEnv {
				t.Errorf("TCELL_ALTSCREEN = %q (present=%v), want %q",
					env, present, tc.wantEnv)
			}
		})
	}
}

// TestFullscreenPrefReadAndAppliedAtLaunch exercises the full launch path end to
// end without a terminal: a persisted Rendering preference is read via
// config.LoadRendering, resolved into Config.FullscreenEnabled (mirroring the cmd
// wiring), carried onto the ControlCenter by New, and applied by the same helper
// Run() invokes before creating the screen. It asserts both the ON and OFF
// preferences reach the tcell flag correctly.
func TestFullscreenPrefReadAndAppliedAtLaunch(t *testing.T) {
	cases := []struct {
		name       string
		fullscreen bool
		wantEnvSet bool // whether TCELL_ALTSCREEN=disable is expected after launch-apply
	}{
		{"saved fullscreen on -> alternate screen", true, false},
		{"saved fullscreen off -> scrollback", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := config.SaveRendering(dir, &config.Rendering{Fullscreen: tc.fullscreen}); err != nil {
				t.Fatalf("SaveRendering: %v", err)
			}
			// Sanity: the file exists so we are exercising the read path, not just
			// the default.
			if _, err := os.Stat(filepath.Join(dir, "rendering.yaml")); err != nil {
				t.Fatalf("expected rendering.yaml written: %v", err)
			}

			// Resolve exactly as cmd/controlcenter.go does.
			cc := New(Config{
				FullscreenEnabled: config.LoadRendering(dir).Fullscreen,
			})
			if cc.fullscreenEnabled != tc.fullscreen {
				t.Fatalf("cc.fullscreenEnabled = %v, want %v", cc.fullscreenEnabled, tc.fullscreen)
			}

			// Apply the same way Run() does, isolated from the real env.
			t.Setenv(tcellAltScreenEnv, "")
			_ = os.Unsetenv(tcellAltScreenEnv)
			applyFullscreenRendering(cc.fullscreenEnabled)

			env, present := os.LookupEnv(tcellAltScreenEnv)
			gotDisabled := present && env == "disable"
			if gotDisabled != tc.wantEnvSet {
				t.Errorf("after launch-apply: TCELL_ALTSCREEN disabled = %v (%q, present=%v), want %v",
					gotDisabled, env, present, tc.wantEnvSet)
			}
		})
	}
}
