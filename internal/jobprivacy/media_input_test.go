package jobprivacy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const mediaFixtureSecret = "synthetic-media.signature"

type privateValueError struct{ detail string }

func (e privateValueError) Error() string {
	return e.detail
}

func (e privateValueError) Unwrap() error {
	return context.DeadlineExceeded
}

func TestReflectedRedactedErrorKeepsClassificationWithoutSecret(t *testing.T) {
	for _, kind := range []string{FileFetch, "OTHER"} {
		payload := map[string]any{"capability": mediaFixtureSecret}
		if kind == "OTHER" {
			payload = map[string]any{PasscodeKey: mediaFixtureSecret}
		}
		original := privateValueError{detail: "failed: " + mediaFixtureSecret}
		safe := Error(kind, payload, original)
		var classified privateValueError
		if !errors.Is(safe, context.DeadlineExceeded) || !errors.As(safe, &classified) || classified != original {
			t.Fatal("redaction lost error identity/classification")
		}
		wrapper, ok := safe.(*redactedError)
		if !ok {
			t.Fatal("fixture was not redacted")
		}
		for _, value := range []any{safe, *wrapper, struct{ private any }{*wrapper}} {
			for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
				if strings.Contains(fmt.Sprintf(format, value), mediaFixtureSecret) {
					t.Fatalf("%s retained original error data under %s", kind, format)
				}
			}
		}
	}
}

func TestPrivateMediaClassificationDoesNotParseOrMutateInputs(t *testing.T) {
	for _, inputs := range []any{
		nil, true, 42, "", "not-json: " + mediaFixtureSecret,
		`{"inputs":[{"capability":"synthetic-media.signature"}]}`,
		`{"inputs":[{"\u0063apability":"synthetic-media.signature"}]}`,
		`{"inputs":[{"capability":"synthetic-media.signature"}],"inputs":null}`,
		[]byte(mediaFixtureSecret), map[string]any{"capability": mediaFixtureSecret},
		strings.Repeat("x", 10<<10) + mediaFixtureSecret,
	} {
		payload := map[string]any{"inputs": inputs, "prompt": "synthetic prompt", PasscodeKey: "private-pin"}
		if !IsPrivateMedia(MediaGenerate, payload) || Payload(MediaGenerate, payload) != nil {
			t.Fatal("malformed/private input bypassed conservative retention")
		}
		if !reflect.DeepEqual(payload["inputs"], inputs) || payload[PasscodeKey] != "private-pin" {
			t.Fatal("retention mutated original execution authority")
		}
		if IsPrivateMedia("OTHER", payload) {
			t.Fatal("unrelated job reclassified as private media")
		}
	}
	if IsPrivateMedia(MediaGenerate, nil) || IsPrivateMedia(MediaGenerate, map[string]any{"prompt": "old text request"}) {
		t.Fatal("legacy text-only job retention changed")
	}
}

func TestPrivateMediaErrorAndFailureFieldsAreStatic(t *testing.T) {
	payload := map[string]any{"inputs": `{"inputs":[{"capability":"` + mediaFixtureSecret + `"}]}`}
	for _, message := range []string{
		"failed " + mediaFixtureSecret,
		fmt.Sprintf("%q", payload["inputs"]),
		fmt.Sprintf("%x", payload["inputs"]),
		privateMediaDiagnostic,
	} {
		original := privateValueError{detail: message + " private-original:" + mediaFixtureSecret}
		safe := Error(MediaGenerate, payload, original)
		if safe.Error() != privateMediaDiagnostic || !errors.Is(safe, context.DeadlineExceeded) {
			t.Fatal("private-media error leaked input or lost classification")
		}
		var actual privateValueError
		if !errors.As(safe, &actual) || actual != original {
			t.Fatal("explicit original error projection changed")
		}
		if Text(MediaGenerate, payload, message) != privateMediaDiagnostic {
			t.Fatal("encoded/malformed private diagnostic bypassed omission")
		}
		data := FailureData(MediaGenerate, payload, original, map[string]any{"error": mediaFixtureSecret, "raw": payload, "output": mediaFixtureSecret})
		if len(data) != 1 || data["error"] != privateMediaDiagnostic {
			t.Fatal("structured failure data retained execution payload")
		}
		wrapper := safe.(*redactedError)
		for _, value := range []any{safe, *wrapper, struct{ private any }{*wrapper}} {
			for _, format := range []string{"%v", "%+v", "%#v", "%q", "%x"} {
				if strings.Contains(fmt.Sprintf(format, value), mediaFixtureSecret) {
					t.Fatal("private-media original error escaped through reflection")
				}
			}
		}
	}
	if Error(MediaGenerate, payload, nil) != nil || FailureData(MediaGenerate, payload, nil, payload)["error"] != "job failed" {
		t.Fatal("nil error semantics changed")
	}
}

func TestLegacyTextMediaAndUnrelatedJobsKeepRetention(t *testing.T) {
	for _, kind := range []string{MediaGenerate, "OTHER"} {
		payload := map[string]any{"prompt": "ordinary prompt", "model": "ordinary-model"}
		original := errors.New("ordinary error")
		if Error(kind, payload, original) != original || !reflect.DeepEqual(Payload(kind, payload), payload) || Text(kind, payload, "ordinary text") != "ordinary text" {
			t.Fatal("legacy job diagnostics changed")
		}
		fields := FailureData(kind, payload, original, map[string]any{"safe": "field"})
		if fields["error"] != "ordinary error" || fields["safe"] != "field" {
			t.Fatal("legacy structured failure fields changed")
		}
	}
}

type privatePanicError struct{}

func (privatePanicError) Error() string {
	panic("private-media diagnostic must not invoke the original formatter")
}

func (privatePanicError) Unwrap() error {
	return context.Canceled
}

func TestPrivateMediaNeverInvokesOriginalErrorFormatter(t *testing.T) {
	payload := map[string]any{"inputs": nil}
	safe := Error(MediaGenerate, payload, privatePanicError{})
	if safe.Error() != privateMediaDiagnostic || !errors.Is(safe, context.Canceled) {
		t.Fatal("private error was not sealed before formatting")
	}
}
