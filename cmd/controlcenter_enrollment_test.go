package cmd

import (
	"errors"
	"reflect"
	"testing"
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
