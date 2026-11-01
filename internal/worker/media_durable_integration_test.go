package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMediaDurableAPIFailureKeepsReceiptAndRedactsFallback(t *testing.T) {
	for _, kind := range []string{"MEDIA_GENERATE", "FILE_FETCH", "CLOCK_FIXTURE"} {
		t.Run(kind, func(t *testing.T) {
			payload := mediaPrivacyRaw()
			if kind == "FILE_FETCH" {
				payload = `{"capability":"` + mediaPrivacyToken + `"}`
			}
			f := newDeliveryFixture(t, kind, payload)
			if kind == "MEDIA_GENERATE" {
				assertMediaJobPrivate(t, f.job)
				if rawOrPayload(f.job) != payload {
					t.Fatal("durable adoption changed receipt input")
				}
			}
			err := f.source.Fail(context.Background(), f.job, errors.New("failure: "+mediaPrivacyToken), map[string]any{"reason": "terminal_failure", "payload": payload})
			if err != nil || len(f.envelopes) != 1 {
				t.Fatal("durable failure lost completion custody", err)
			}
			containsPrivate := strings.Contains(string(f.envelopes[0]), mediaPrivacyToken)
			if containsPrivate != (kind == "CLOCK_FIXTURE") {
				t.Fatal("durable fallback violated the job privacy policy")
			}
			f.check(t) // A legacy status/ACK route is not permitted by this fixture.
		})
	}
}

func TestMediaDurableAPINackLapsesWithoutLegacyStatus(t *testing.T) {
	f := newDeliveryFixture(t, "MEDIA_GENERATE", mediaPrivacyRaw())
	if err := f.source.Nack(context.Background(), f.job, errors.New(mediaPrivacyToken)); err != nil {
		t.Fatal(err)
	}
	if len(f.envelopes) != 0 || f.job.delivery().valid() {
		t.Fatal("retry was committed or retained an executable lease")
	}
	assertMediaJobPrivate(t, f.job)
	f.check(t)
}
