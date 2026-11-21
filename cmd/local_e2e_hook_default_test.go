//go:build !citadel_e2e

package cmd

import "testing"

// A shipped build must never carry the test seam: only local_e2e_hook.go, built
// with the citadel_e2e tag, may set it.
func TestLocalE2EHookIsNilInDefaultBuild(t *testing.T) {
	if localE2EHook != nil {
		t.Fatal("localE2EHook is set in a build without the citadel_e2e tag")
	}
}
