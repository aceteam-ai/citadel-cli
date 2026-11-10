package redact

import (
	"strings"
	"testing"
)

func TestFingerprint(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "", "<empty>"},
		{"one char", "a", "<redacted>"},
		{"fifteen chars", "123456789012345", "<redacted>"},
		{"sixteen chars", "1234567890123456", "…3456"},
		{"long", "sk-abcdefghijklmnopqrstuvwxyz1234", "…1234"},
		{"unicode fifteen runes", strings.Repeat("é", 15), "<redacted>"},
		{"unicode sixteen runes", strings.Repeat("é", 12) + "日本語字", "…日本語字"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Fingerprint(tt.in); got != tt.want {
				t.Fatalf("Fingerprint(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsSensitiveName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Authorization", true},
		{"PROXY-AUTHORIZATION", true},
		{"Cookie", true},
		{"Set-Cookie", true},
		{"X-API-Key", true},
		{"access_token", true},
		{"client_secret", true},
		{"apikey", true},
		{"Password", true},
		{"passwd", true},
		{"sig", true},
		{"X-Signature", true},
		{"credentials", true},
		{"sessionid", true},
		{"Content-Type", false},
		{"Accept", false},
		{"page", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSensitiveName(tt.name); got != tt.want {
				t.Fatalf("IsSensitiveName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestHeader(t *testing.T) {
	secret := "abcdefghijklmnopqrstuvwxyz0123a1b2"
	tests := []struct {
		name, hname, value, want string
	}{
		{"bearer", "Authorization", "Bearer " + secret, "Bearer …a1b2"},
		{"mixed case proxy auth bearer", "proxy-authorization", "bearer " + secret, "bearer …a1b2"},
		{"basic has no tail", "Authorization", "Basic " + secret, "Basic <redacted>"},
		{"basic lowercase", "proxy-authorization", "basic " + secret, "basic <redacted>"},
		{"basic upper", "Authorization", "BASIC dXNlcjpwdw==", "BASIC <redacted>"},
		{"no scheme", "Authorization", secret, "…a1b2"},
		{"short token with scheme", "Authorization", "Bearer abc", "Bearer <redacted>"},
		{"empty authorization", "Authorization", "", "<empty>"},
		{"api key", "X-API-Key", secret, "…a1b2"},
		{"short api key", "x-api-key", "short", "<redacted>"},
		{"cookie", "Cookie", "session=" + secret, "…a1b2"},
		{"plain", "Content-Type", "application/json", "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Header(tt.hname, tt.value)
			if got != tt.want {
				t.Fatalf("Header(%q, %q) = %q, want %q", tt.hname, tt.value, got, tt.want)
			}
			if len(tt.value) >= 16 && strings.Contains(got, tt.value) && tt.want != tt.value {
				t.Fatalf("full value leaked: %q", got)
			}
		})
	}
}

func TestURL(t *testing.T) {
	secret := "abcdefghijklmnopqrstuvwxyz0123a1b2"
	tests := []struct {
		name, in, want string
	}{
		{"no secrets", "https://example.com/a/b?x=1&y=2", "https://example.com/a/b?x=1&y=2"},
		{"userinfo password", "http://alice:" + secret + "@host:8000/p", "http://alice:…a1b2@host:8000/p"},
		{"userinfo short password", "http://alice:pw@host/p", "http://alice:<redacted>@host/p"},
		{"username only", "http://alice@host/p", "http://alice@host/p"},
		{"git username kept", "ssh://git@host/org/repo.git", "ssh://git@host/org/repo.git"},
		{"token as username", "https://ghp_" + secret + "@github.com/org/repo.git", "https://…a1b2@github.com/org/repo.git"},
		{"token username and oauth password", "https://" + secret + ":x-oauth-basic@host/p", "https://…a1b2:<redacted>@host/p"},
		{"short username with long password", "http://alice:" + secret + "@host/p", "http://alice:…a1b2@host/p"},
		{"fifteen rune username kept", "http://123456789012345@host/p", "http://123456789012345@host/p"},
		{"token param", "http://h/sql?token=" + secret, "http://h/sql?token=…a1b2"},
		{"mixed case names", "http://h/?API_Key=" + secret + "&Access_Token=" + secret + "&SIG=" + secret + "&q=hello", "http://h/?API_Key=…a1b2&Access_Token=…a1b2&SIG=…a1b2&q=hello"},
		{"short secret param", "http://h/?key=abc", "http://h/?key=<redacted>"},
		{"empty secret param", "http://h/?key=&q=1", "http://h/?key=<empty>&q=1"},
		{"fragment dropped", "http://h/p#access_token=" + secret, "http://h/p#redacted"},
		{"unparseable", "http://[::1", "<unparseable url>"},
		{"control char", "http://h/\x7f%zz", "<unparseable url>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := URL(tt.in)
			if got != tt.want {
				t.Fatalf("URL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.Contains(got, secret) {
				t.Fatalf("full secret leaked: %q", got)
			}
		})
	}
}
