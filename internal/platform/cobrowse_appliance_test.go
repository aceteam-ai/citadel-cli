package platform

import (
	"strings"
	"testing"
)

func TestApplianceWindowSizeIsOptIn(t *testing.T) {
	t.Setenv(EnvCobrowseResolution, "")
	t.Cleanup(func() { SetBrowserWindowSize("") })
	SetBrowserWindowSize("")
	before := strings.Join(buildChromeArgs(cobrowseLaunchOptions{}), " ")
	if !strings.Contains(before, "--start-maximized") || strings.Contains(before, "--window-size=") {
		t.Fatal(before)
	}
	SetBrowserWindowSize("1280,720")
	if got := xvfbResolution(); got != "1280x720x24" {
		t.Fatal(got)
	}
	for _, headless := range []bool{false, true} {
		args := strings.Join(buildChromeArgs(cobrowseLaunchOptions{headless: headless}), " ")
		if strings.Contains(args, "--start-maximized") || !strings.Contains(args, "--window-size=1280,720") {
			t.Fatal(args)
		}
	}
	SetBrowserWindowSize("")
	if got := xvfbResolution(); got != defaultXvfbResolution {
		t.Fatal(got)
	}
	if after := strings.Join(buildChromeArgs(cobrowseLaunchOptions{}), " "); before != after {
		t.Fatal("unset profile must preserve defaults")
	}
}

func TestApplianceWindowKeepsExplicitDisplayOverride(t *testing.T) {
	t.Cleanup(func() { SetBrowserWindowSize("") })
	SetBrowserWindowSize("1280,720")
	t.Setenv(EnvCobrowseResolution, "1600x900x24")
	if got := xvfbResolution(); got != "1600x900x24" {
		t.Fatal(got)
	}
}
