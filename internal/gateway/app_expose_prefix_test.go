package gateway

import "testing"

// TestIsAppExposeName pins the reserved hosted-app exposure namespace (CRAM A1):
// "app-<short_code>" routes are the app runner's alone; the operator expose
// verbs refuse them.
func TestIsAppExposeName(t *testing.T) {
	if AppExposePrefix != "app-" {
		t.Fatalf("AppExposePrefix = %q, want app-", AppExposePrefix)
	}
	reserved := []string{"app-ac-blue-cat-fox", "app-", "app-x"}
	for _, n := range reserved {
		if !IsAppExposeName(n) {
			t.Errorf("IsAppExposeName(%q) = false, want true", n)
		}
	}
	notReserved := []string{"frigate", "grafana", "myapp", "ap", ""}
	for _, n := range notReserved {
		if IsAppExposeName(n) {
			t.Errorf("IsAppExposeName(%q) = true, want false", n)
		}
	}
	// The route the app runner mints must satisfy the exposure name grammar.
	if !ValidExposeName(AppExposePrefix + "ac-blue-cat-fox") {
		t.Error("app-<short_code> must be a valid exposure name")
	}
}
