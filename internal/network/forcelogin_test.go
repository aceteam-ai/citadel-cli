package network

import (
	"os"
	"testing"

	"tailscale.com/envknob"
)

// tsnet uses the auth key on a node with saved state only when
// TSNET_FORCE_LOGIN is set, and reads it at Start time.
func TestForceLoginEnvScopedToOneStart(t *testing.T) {
	t.Setenv("TSNET_FORCE_LOGIN", "")
	var during bool
	if err := withForceLogin(true, func() error {
		during = envknob.Bool("TSNET_FORCE_LOGIN")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !during {
		t.Fatal("force login must be visible to tsnet during Start")
	}
	if os.Getenv("TSNET_FORCE_LOGIN") != "" || envknob.Bool("TSNET_FORCE_LOGIN") {
		t.Fatal("force login must not outlive the Start")
	}
}

func TestForceLoginOffLeavesEnvAlone(t *testing.T) {
	t.Setenv("TSNET_FORCE_LOGIN", "")
	_ = withForceLogin(false, func() error {
		if envknob.Bool("TSNET_FORCE_LOGIN") {
			t.Fatal("must not force login unless asked")
		}
		return nil
	})
}

func TestForceLoginRefusedWhenAttached(t *testing.T) {
	s := NewServer(ServerConfig{ForceLogin: true})
	if err := s.checkForceLoginMode(ModeAttached); err == nil {
		t.Fatal("an attached backend ignores the auth key; force login must refuse")
	}
	if err := s.checkForceLoginMode(ModeUserspace); err != nil {
		t.Fatal(err)
	}
}
