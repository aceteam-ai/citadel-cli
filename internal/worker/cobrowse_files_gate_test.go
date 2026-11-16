package worker

import "testing"

func TestCobrowseSessionHandlerFollowsLiveFilesPermission(t *testing.T) {
	on := false
	h := cobrowseSessionHandlerFor(LegacyHandlerOpts{FilesEnabled: func() bool { return on }})
	if h.Enabled == nil || h.Enabled() {
		t.Fatal("handler must read the live Files permission, currently off")
	}
	on = true
	if !h.Enabled() {
		t.Fatal("handler must see Files turned on without being rebuilt")
	}
	if !cobrowseSessionHandlerFor(LegacyHandlerOpts{FilesDisabled: true}).Disabled {
		t.Fatal("static FilesDisabled must carry over")
	}
}
