package reconcile

import "testing"

func TestPullEnabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"false", false},
		{"0", false},
		{"off", false},
		{"nope", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{" yes ", true},
		{"on", true},
	}
	for _, c := range cases {
		t.Setenv(PullEnvVar, c.val)
		if got := PullEnabled(); got != c.want {
			t.Errorf("PullEnabled() with %q = %v, want %v", c.val, got, c.want)
		}
	}
}

func TestPullEnabledDefaultsOffWhenUnset(t *testing.T) {
	// Ensure the var is not inherited from the environment.
	t.Setenv(PullEnvVar, "")
	if PullEnabled() {
		t.Fatal("pull loop must default OFF when unset")
	}
}
