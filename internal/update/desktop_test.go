package update

import "testing"

func TestDesktopManagedPaths(t *testing.T) {
	for _, path := range []string{
		"/Applications/Citadel.app/Contents/MacOS/citadel",
		"/private/var/folders/translocated/Citadel.app/Contents/MacOS/citadel-aarch64-apple-darwin",
		"/Users/alice/Library/Application Support/ai.aceteam.citadel/helpers/abc123/citadel",
	} {
		if !IsDesktopManagedPath(path, "darwin") {
			t.Errorf("desktop helper not recognized: %s", path)
		}
		if IsDesktopManagedPath(path, "linux") {
			t.Errorf("non-macOS helper recognized: %s", path)
		}
	}
	if IsDesktopManagedPath("/usr/local/bin/citadel", "darwin") {
		t.Fatal("standalone CLI should keep its own update lifecycle")
	}
}
