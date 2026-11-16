//go:build !windows

package update

import "testing"

func ensureFixtureOwnedByCurrentUser(t *testing.T, path string) { t.Helper() }
