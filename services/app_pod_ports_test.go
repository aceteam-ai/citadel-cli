package services

import "testing"

func TestAllocateAppPodPort(t *testing.T) {
	t.Run("empty node gets the range start", func(t *testing.T) {
		p, err := AllocateAppPodPort(func(int) bool { return false })
		if err != nil {
			t.Fatal(err)
		}
		if p != AppPodPortRangeStart {
			t.Errorf("got %d, want %d", p, AppPodPortRangeStart)
		}
	})

	t.Run("skips in-use ports", func(t *testing.T) {
		used := map[int]bool{AppPodPortRangeStart: true, AppPodPortRangeStart + 1: true}
		p, err := AllocateAppPodPort(func(x int) bool { return used[x] })
		if err != nil {
			t.Fatal(err)
		}
		if p != AppPodPortRangeStart+2 {
			t.Errorf("got %d, want %d", p, AppPodPortRangeStart+2)
		}
	})

	t.Run("exhausted range errors", func(t *testing.T) {
		if _, err := AllocateAppPodPort(func(int) bool { return true }); err == nil {
			t.Error("expected an error when every port is in use")
		}
	})

	t.Run("range is clear of the reserved/module/apps ports", func(t *testing.T) {
		// The hosted-app range must not overlap citadel's own listeners, the
		// module 8200 block, or the apps catalog range.
		if AppPodPortRangeStart <= AppsPortRangeEnd {
			t.Error("app pod range overlaps the apps catalog range")
		}
		for p := AppPodPortRangeStart; p <= AppPodPortRangeEnd; p++ {
			if _, reserved := ReservedCitadelPorts[p]; reserved {
				t.Errorf("app pod range includes reserved port %d", p)
			}
		}
	})
}
