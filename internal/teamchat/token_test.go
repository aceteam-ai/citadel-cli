package teamchat

import "testing"

func TestResolveToken(t *testing.T) {
	cases := []struct {
		name       string
		env        string
		config     string
		device     string
		wantToken  string
		wantSource string
	}{
		{"env wins", "act_env", "act_cfg", "act_dev", "act_env", TokenSourceEnv},
		{"config over device", "", "act_cfg", "act_dev", "act_cfg", TokenSourceConfig},
		{"device fallback", "", "", "act_dev", "act_dev", TokenSourceDevice},
		{"nothing", "", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, source := ResolveToken(tc.env, tc.config, tc.device)
			if token != tc.wantToken || source != tc.wantSource {
				t.Errorf("ResolveToken = (%q, %q), want (%q, %q)",
					token, source, tc.wantToken, tc.wantSource)
			}
		})
	}
}
