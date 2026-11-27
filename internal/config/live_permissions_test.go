package config

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLivePolicyWatch(t *testing.T) {
	for _, change := range []string{"disable", "rotate", "unreadable", "nil"} {
		t.Run(change, func(t *testing.T) {
			var state atomic.Int32
			source := PermissionSource(func() (*Permissions, error) {
				p := &Permissions{Console: true, PasscodeHash: "initial"}
				if state.Load() != 0 {
					switch change {
					case "disable":
						p.Console = false
					case "rotate":
						p.PasscodeHash = "updated"
					case "unreadable":
						return nil, errors.New("fixture")
					case "nil":
						return nil, nil
					}
				}
				return p, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			revoked := make(chan struct{})
			go source.Watch(ctx, "console", "initial", func() { close(revoked) })
			select {
			case <-revoked:
				t.Fatal("revoked permitted policy")
			case <-time.After(20 * time.Millisecond):
			}
			state.Store(1)
			select {
			case <-revoked:
			case <-time.After(2 * time.Second):
				t.Fatal("did not revoke")
			}
		})
	}
}

func TestLivePolicyWatchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		PermissionSource(func() (*Permissions, error) { return &Permissions{Services: true}, nil }).Watch(ctx, "services", "", func() { t.Error("revoked after cancellation") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher remained active")
	}
}

func TestLivePolicyCapabilities(t *testing.T) {
	for _, category := range []string{"console", "desktop", "files", "services", "ssh", "provision", "shell"} {
		t.Run(category, func(t *testing.T) {
			if CapabilityEnabled(nil, category) || CapabilityEnabled(&Permissions{}, category) {
				t.Fatal("disabled capability permitted")
			}
			p := &Permissions{Console: true, Desktop: true, Files: true, Services: true, SSH: true, Provision: true, Shell: true}
			if !CapabilityEnabled(p, category) {
				t.Fatal("enabled capability refused")
			}
		})
	}
	if !CapabilityEnabled(&Permissions{}, "") {
		t.Fatal("uncategorized route changed")
	}
}
