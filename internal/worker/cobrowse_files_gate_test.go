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
	// No permission source at all denies.
	if cobrowseSessionHandlerFor(LegacyHandlerOpts{}).Enabled != nil {
		t.Fatal("no live source must leave the handler denying")
	}
}
