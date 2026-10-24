package cmd

import (
	"errors"
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestControlCenterEnrollmentRequiresCurrentGrantLink(t *testing.T) {
	for _, tc := range []struct {
		name       string
		refuseLink bool
	}{{"linked", false}, {"refused", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			refused := errors.New("reservation proof does not match registered node")
			err := ccConnectDeviceEnrollmentWith("current-registration-key", "current-modal-grant", func() error {
				events = append(events, "register")
				return nil
			}, func(result *DeviceAuthResult) error {
				events = append(events, "link")
				if result.DeviceCode != "current-modal-grant" || result.Token.Authkey != "current-registration-key" {
					t.Fatal("Control Center lost or substituted the approved modal's grant")
				}
				if tc.refuseLink {
					return refused
				}
				return nil
			}, func() error {
				events = append(events, "disconnect")
				return nil
			})
			want := []string{"register", "link"}
			if tc.refuseLink {
				want = append(want, "disconnect")
				if !errors.Is(err, refused) {
					t.Fatal("refused link must reach the UI as an error")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("enrollment events = %v, want %v", events, want)
			}
		})
	}
}

func TestControlCenterEnrollmentNeverLinksSavedCredentialsWithoutCurrentRegistration(t *testing.T) {
	for _, args := range [][2]string{{"", "current-grant"}, {"current-registration-key", ""}} {
		err := ccConnectDeviceEnrollmentWith(args[0], args[1], func() error {
			t.Fatal("missing current authorization must fail before connecting")
			return nil
		}, func(*DeviceAuthResult) error {
			t.Fatal("saved device credentials must not substitute for this grant")
			return nil
		}, func() error {
			t.Fatal("must not disconnect an identity this attempt never connected")
			return nil
		})
		if err == nil {
			t.Fatal("missing modal grant or registration key must fail")
		}
	}
	connectErr := errors.New("registration failed")
	err := ccConnectDeviceEnrollmentWith("current-key", "current-grant", func() error { return connectErr }, func(*DeviceAuthResult) error {
		t.Fatal("failed registration must never link a previous node")
		return nil
	}, func() error {
		t.Fatal("must not disconnect an identity this attempt never connected")
		return nil
	})
	if !errors.Is(err, connectErr) {
		t.Fatal("registration error must propagate")
	}
}

func TestControlCenterEnrollmentResetsPreviousNodeBeforeInjectedConnect(t *testing.T) {
	readID := func() string {
		registeredNodeID.Lock()
		defer registeredNodeID.Unlock()
		return registeredNodeID.id
	}
	previous := readID()
	t.Cleanup(func() { setRegisteredNodeID(previous) })
	for _, mode := range []string{"current-node", "failed-connect", "missing-node"} {
		t.Run(mode, func(t *testing.T) {
			setRegisteredNodeID("fixture-stale-node")
			connectErr := errors.New("fixture connect failed")
			linkErr := errors.New("fixture registration missing")
			links, disconnects := 0, 0
			err := ccConnectDeviceEnrollmentWith("fixture-key", "fixture-grant", func() error {
				if readID() != "" {
					t.Fatal("previous registration survived into connect")
				}
				if mode == "failed-connect" {
					return connectErr
				}
				if mode == "current-node" {
					setRegisteredNodeID("fixture-current-node")
				}
				return nil
			}, func(*DeviceAuthResult) error {
				links++
				if mode == "missing-node" {
					if readID() != "" {
						t.Fatal("missing registration inherited a stale node")
					}
					return linkErr
				}
				if mode != "current-node" || readID() != "fixture-current-node" {
					t.Fatal("link did not select this connect's registration")
				}
				return nil
			}, func() error { disconnects++; return nil })
			switch mode {
			case "current-node":
				if err != nil || links != 1 || disconnects != 0 {
					t.Fatal("current registration did not link normally")
				}
			case "failed-connect":
				if !errors.Is(err, connectErr) || links != 0 || disconnects != 0 || readID() != "" {
					t.Fatal("failed connect reused or linked a previous registration")
				}
			case "missing-node":
				if !errors.Is(err, linkErr) || links != 1 || disconnects != 1 {
					t.Fatal("missing registration did not follow the link-refusal path")
				}
			}
		})
	}
}

func TestControlCenterServerRetryUsesTypedPendingAndResetsOnProtocolAnswer(t *testing.T) {
	const grant = "fixture-typed-grant"
	t.Cleanup(func() { ccTokenServerErrors.Delete(grant) })
	serverErr := &nexus.TokenServerError{StatusCode: 503}
	for i := 0; i < nexus.MaxConsecutiveTokenServerErrors; i++ {
		err := ccTokenPollError(grant, serverErr)
		var pending *nexus.TokenError
		if !errors.As(err, &pending) || pending == nil || pending.ErrorCode != "authorization_pending" {
			t.Fatal("bounded server retry lost typed pending authority")
		}
	}
	slow := &nexus.TokenError{ErrorCode: "slow_down"}
	if ccTokenPollError(grant, slow) != slow {
		t.Fatal("slowdown must reach the modal unchanged")
	}
	var pending *nexus.TokenError
	if !errors.As(ccTokenPollError(grant, serverErr), &pending) || pending == nil || pending.ErrorCode != "authorization_pending" {
		t.Fatal("protocol answer did not reset the server-error streak")
	}
}
