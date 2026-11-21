package platform

import (
	"errors"
	"testing"
)

func TestValidateCobrowseURL(t *testing.T) {
	allowed := []string{
		"",
		"about:blank",
		"https://example.com",
		"http://example.com/path?q=1#frag",
		"HTTPS://EXAMPLE.COM",
		"http://127.0.0.1:8080/",
	}
	for _, raw := range allowed {
		if err := ValidateCobrowseURL(raw); err != nil {
			t.Errorf("ValidateCobrowseURL(%q) = %v, want nil", raw, err)
		}
	}
	refused := []string{
		"--some-switch=value",
		"-x",
		"/flag=value",
		"file:///etc/passwd",
		"data:text/html,hi",
		"javascript:alert(1)",
		"chrome://settings",
		"about:config",
		"https://",
		"https:example.com",
		"example.com",
		"https://example.com/ two",
		"https://example.com/\n",
		"https://exa\x00mple.com",
	}
	for _, raw := range refused {
		if err := ValidateCobrowseURL(raw); !errors.Is(err, ErrCobrowseURLNotAllowed) {
			t.Errorf("ValidateCobrowseURL(%q) = %v, want ErrCobrowseURLNotAllowed", raw, err)
		}
	}
}

func TestCobrowseSessionRefusesDisallowedURLs(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	if _, err := m.StartSession("--some-switch=value"); !errors.Is(err, ErrCobrowseURLNotAllowed) {
		t.Fatalf("StartSession(switch) = %v, want ErrCobrowseURLNotAllowed", err)
	}
	if _, err := m.Navigate("any", "file:///etc/passwd"); !errors.Is(err, ErrCobrowseURLNotAllowed) {
		t.Fatalf("Navigate(file) = %v, want ErrCobrowseURLNotAllowed", err)
	}
	var legacy CobrowseManager
	if _, err := legacy.Start(t.TempDir(), "/flag=value", 0); !errors.Is(err, ErrCobrowseURLNotAllowed) {
		t.Fatalf("legacy Start(flag) = %v, want ErrCobrowseURLNotAllowed", err)
	}
	if _, err := legacy.Navigate("javascript:alert(1)"); !errors.Is(err, ErrCobrowseURLNotAllowed) {
		t.Fatalf("legacy Navigate(javascript) = %v, want ErrCobrowseURLNotAllowed", err)
	}
}
