package reconcile

import "testing"

func TestPullDisabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		// Kill switch: only explicit falsy tokens disable.
		{"false", true},
		{"0", true},
		{"off", true},
		{"no", true},
		{"FALSE", true},
		{" off ", true},
		// Default-on: unset or anything non-falsy keeps the loop enabled.
		{"", false},
		{"1", false},
		{"true", false},
		{"on", false},
		{"nope", false},
	}
	for _, c := range cases {
		t.Setenv(PullEnvVar, c.val)
		if got := PullDisabled(); got != c.want {
			t.Errorf("PullDisabled() with %q = %v, want %v", c.val, got, c.want)
		}
	}
}

func TestPullDefaultsOnWhenUnset(t *testing.T) {
	// Ensure the var is not inherited from the environment.
	t.Setenv(PullEnvVar, "")
	if PullDisabled() {
		t.Fatal("pull loop must default ON when unset (kill switch not engaged)")
	}
}
