package controlcenter

import (
	"errors"
	"reflect"
	"testing"
)

func TestDeviceEnrollmentRefusalSuppressesSuccessAndWorkerCallback(t *testing.T) {
	refused := errors.New("registered node did not match the current enrollment reservation")
	var events []string
	err := completeDeviceEnrollment("current-authkey", "current-modal-grant", func(authkey, code string) error {
		if authkey != "current-authkey" || code != "current-modal-grant" {
			t.Fatal("connect must receive the grant from the approved modal")
		}
		events = append(events, "link-refused")
		return refused
	}, func() {
		events = append(events, "success-display", "worker-start")
	})
	if !errors.Is(err, refused) || !reflect.DeepEqual(events, []string{"link-refused"}) {
		t.Fatalf("refused link must prevent success and worker callbacks: err=%v events=%v", err, events)
	}
}

func TestDeviceEnrollmentSuccessCallbackFollowsCompletedLink(t *testing.T) {
	var events []string
	err := completeDeviceEnrollment("current-authkey", "current-modal-grant", func(authkey, code string) error {
		events = append(events, "registration-and-link-complete")
		return nil
	}, func() {
		events = append(events, "success-display", "worker-start")
	})
	if err != nil || !reflect.DeepEqual(events, []string{"registration-and-link-complete", "success-display", "worker-start"}) {
		t.Fatalf("success actions must follow linking: err=%v events=%v", err, events)
	}
}

func TestDeviceEnrollmentMissingConnectorNeverReportsSuccess(t *testing.T) {
	if err := completeDeviceEnrollment("current-key", "current-grant", nil, func() {
		t.Fatal("missing enrollment connector must not invoke success callback")
	}); err == nil {
		t.Fatal("missing enrollment connector must fail explicitly")
	}
}

func TestDeviceEnrollmentMissingGrantNeverConnectsOrReportsSuccess(t *testing.T) {
	for _, args := range [][2]string{{"", "current-grant"}, {"current-key", ""}} {
		if err := completeDeviceEnrollment(args[0], args[1], func(string, string) error {
			t.Fatal("missing current authorization must not reach network connection")
			return nil
		}, func() {
			t.Fatal("missing current authorization must not invoke success callback")
		}); err == nil {
			t.Fatal("missing current authorization must fail explicitly")
		}
	}
}
