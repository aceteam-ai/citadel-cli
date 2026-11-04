package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureLLMPolicyWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func assertLLMPolicyWarning(t *testing.T, output *bytes.Buffer, reason, message, privatePath string) {
	t.Helper()
	var record struct {
		Level   string `json:"level"`
		Message string `json:"msg"`
		Reason  string `json:"reason"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	if err := decoder.Decode(&record); err != nil || record.Level != "WARN" || record.Message != message || record.Reason != reason {
		t.Fatalf("missing bounded policy diagnostic: record=%+v err=%v", record, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("policy load emitted more than one warning or invalid diagnostic data")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 || fields["time"] == nil || fields["level"] == nil || fields["msg"] == nil || fields["reason"] == nil {
		t.Fatal("policy warning gained an unreviewed diagnostic field")
	}
	for _, private := range []string{privatePath, "private-marker", "http://", "169.254.169.254", LLMPolicyFile} {
		if strings.Contains(output.String(), private) {
			t.Fatal("policy diagnostic exposed a path, endpoint or document content")
		}
	}
}

func TestLoadLLMPolicyMalformedWarningUsesActualStaticReason(t *testing.T) {
	for _, data := range []string{"", " \n\t", "invalid: private-marker"} {
		t.Run("malformed", func(t *testing.T) {
			output := captureLLMPolicyWarnings(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, LLMPolicyFile), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			p := LoadLLMPolicy(dir)
			if p.loadErr == nil || p.loadWhy != "invalid_policy_document" {
				t.Fatal("malformed document did not retain the fail-closed policy")
			}
			for _, endpoint := range []string{defaultLLMEndpoint, "http://8.8.8.8/api/chat"} {
				if _, _, err := p.resolveURL(context.Background(), endpoint); err == nil {
					t.Fatal("malformed policy authorized a destination")
				}
			}
			assertLLMPolicyWarning(t, output, "invalid_policy_document", "workflow LLM policy unreadable or invalid; LLM destinations disabled", dir)
		})
	}
}

func TestLoadLLMPolicyIneffectiveLiteralGrantsWarnWithoutChangingAcceptance(t *testing.T) {
	for _, address := range []string{
		"169.254.169.254", "0.0.0.0", "224.0.0.1", "255.255.255.255",
		"100.100.100.100", "fd7a:115c:a1e0::53", "198.18.0.1", "192.0.2.1",
		"fe80::1", "fec0::1", "2001:db8::1", "64:ff9b::7f00:1", "2002:7f00:1::1", "::ffff:0:7f00:1",
	} {
		t.Run(address, func(t *testing.T) {
			output := captureLLMPolicyWarnings(t)
			dir := t.TempDir()
			endpoint := "http://" + net.JoinHostPort(address, "11434") + "/private-marker"
			const mesh = "http://100.64.0.58:11434/api/chat"
			data := "allow:\n  - endpoint: " + endpoint + "\n  - endpoint: http://169.254.169.254:11435/another-private-marker\n  - endpoint: " + mesh + "\n"
			if err := os.WriteFile(filepath.Join(dir, LLMPolicyFile), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			p := LoadLLMPolicy(dir)
			if p.loadErr != nil || len(p.endpoints) != 3 {
				t.Fatal("ineffective literal grant changed document acceptance")
			}
			if _, _, err := p.resolveURL(context.Background(), endpoint); err == nil {
				t.Fatal("ineffective literal grant authorized its destination")
			}
			if _, _, err := p.resolveURL(context.Background(), defaultLLMEndpoint); err != nil {
				t.Fatal("ineffective literal grant removed the built-in default")
			}
			if _, _, err := p.resolveURL(context.Background(), mesh); err != nil {
				t.Fatal("ineffective literal grant removed another effective grant")
			}
			assertLLMPolicyWarning(t, output, "ineffective_literal_ip_grant", "workflow LLM policy contains an ineffective literal-IP grant; destination remains denied", dir)
		})
	}
}

func TestLoadLLMPolicyBenignAndMissingPoliciesDoNotWarnOrResolve(t *testing.T) {
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		t.Error("policy loading attempted DNS")
		return nil, errors.New("owned resolver trap")
	}}
	t.Cleanup(func() { net.DefaultResolver = previous })
	for _, data := range []string{
		"missing", "missing-directory", "allow: []\n",
		"allow: [{endpoint: 'http://127.0.0.1:11434/api/chat'}]",
		"allow: [{endpoint: 'http://[::1]:11434/api/chat'}]",
		"allow: [{endpoint: 'http://192.168.1.2:11434/api/chat'}]",
		"allow: [{endpoint: 'http://100.64.0.58:11434/api/chat'}]",
		"allow: [{endpoint: 'http://[fc00::1]:11434/api/chat'}]",
		"allow: [{endpoint: 'https://8.8.8.8/api/chat'}]",
		"allow: [{endpoint: 'http://private-marker.invalid:11434/api/chat'}]",
	} {
		t.Run("benign", func(t *testing.T) {
			output := captureLLMPolicyWarnings(t)
			dir := t.TempDir()
			if data == "missing-directory" {
				dir = filepath.Join(dir, "absent-node-directory")
			}
			if data != "missing" && data != "missing-directory" {
				if err := os.WriteFile(filepath.Join(dir, LLMPolicyFile), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p := LoadLLMPolicy(dir)
			if p.loadErr != nil {
				t.Fatal("benign policy failed to load")
			}
			if _, _, err := p.resolveURL(context.Background(), defaultLLMEndpoint); err != nil {
				t.Fatal("benign or missing policy removed the built-in default")
			}
			if output.Len() != 0 {
				t.Fatal("benign or missing policy produced a warning")
			}
		})
	}
}
