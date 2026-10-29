package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

func TestDeviceLinkDiagnosticReportsUncertaintyWithoutCredentials(t *testing.T) {
	for _, dc := range []*DeviceConfig{nil, {}, {RedisURL: "redis://legacy-secret@invalid"}} {
		var out bytes.Buffer
		printDeviceLinkDiagnosticContext(context.Background(), &out, dc)
		if out.Len() != 0 {
			t.Fatalf("non-API configuration acquired a device-link claim: %q", out.String())
		}
	}
	dc := &DeviceConfig{DeviceAPIToken: "device-secret", APIBaseURL: "https://private-origin.invalid", OrgID: "private-org"}
	var out bytes.Buffer
	printDeviceLinkDiagnosticContext(context.Background(), &out, dc)
	got := out.String()
	for _, want := range []string{"Device link: unverified", "saved credentials do not prove", "citadel logout && citadel init", "fresh browser approval"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic lacks %q: %q", want, got)
		}
	}
	for _, secret := range []string{dc.DeviceAPIToken, dc.APIBaseURL, dc.OrgID} {
		if strings.Contains(got, secret) {
			t.Fatal("diagnostic exposed configuration content")
		}
	}
}

func TestDeviceLinkDiagnosticPreservesPreferredCredentialAndFiles(t *testing.T) {
	preferred, legacy := t.TempDir(), t.TempDir()
	files := make(map[string][]byte)
	files[filepath.Join(preferred, "config.yaml")] = []byte("device_api_token: preferred-secret\norg_id: org-a\n")
	files[filepath.Join(legacy, "config.yaml")] = []byte("device_api_token: legacy-secret\norg_id: org-b\n")
	for path, data := range files {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	dc := readDeviceConfigFromDirs([]string{preferred, legacy})
	if dc == nil || dc.DeviceAPIToken != "preferred-secret" {
		t.Fatal("preferred credential was not selected")
	}
	var out bytes.Buffer
	printDeviceLinkDiagnosticContext(context.Background(), &out, dc)
	if !strings.Contains(out.String(), "unverified") {
		t.Fatal("saved credential was silently treated as proof of a link")
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("observation changed a credential file")
		}
	}
	if dc.DeviceAPIToken != "preferred-secret" {
		t.Fatal("observation discarded the preferred credential or exposed legacy fallback")
	}
}

func TestConnectWithBackoffAuthorizationIsVisibleSanitizedAndStillRetries(t *testing.T) {
	withFastBackoff(t)
	original := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = original
		_ = w.Close()
		_ = r.Close()
	})
	connector := &mockConnector{errs: []error{
		fmt.Errorf("must-not-print-secret: %w", &redisapi.AuthorizationError{StatusCode: 401}),
		&redisapi.AuthorizationError{StatusCode: 403},
		nil,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := connectWithBackoffLabeled(ctx, "test", connector); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	os.Stderr = original
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if connector.calls != 3 {
		t.Fatalf("authorization refusal stopped normal recovery: calls=%d", connector.calls)
	}
	if strings.Count(got, "API authorization:") != 1 || !strings.Contains(got, "citadel logout && citadel init") {
		t.Fatalf("default output lacks bounded actionable diagnosis: %q", got)
	}
	if strings.Contains(got, "must-not-print-secret") {
		t.Fatal("default output exposed the wrapping error")
	}
}
