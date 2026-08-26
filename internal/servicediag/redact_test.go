package servicediag

import "testing"

func TestRedactEnv(t *testing.T) {
	in := map[string]string{
		"API_KEY":       "sk-abc123",
		"AUTH_TOKEN":    "tok-xyz",
		"HF_TOKEN":      "hf_secret",
		"DB_PASSWORD":   "hunter2",
		"MY_SECRET_VAL": "s3cr3t",
		"CREDENTIAL_ID": "cred-1",
		"PORT":          "8213",
		"LOG_LEVEL":     "info",
		"MODEL_NAME":    "Qwen/Qwen3-8B",
	}
	out := RedactEnv(in)

	secretKeys := []string{"API_KEY", "AUTH_TOKEN", "HF_TOKEN", "DB_PASSWORD", "MY_SECRET_VAL", "CREDENTIAL_ID"}
	for _, k := range secretKeys {
		if out[k] != redactedPlaceholder {
			t.Errorf("out[%s] = %q, want redacted", k, out[k])
		}
	}

	plainKeys := map[string]string{"PORT": "8213", "LOG_LEVEL": "info", "MODEL_NAME": "Qwen/Qwen3-8B"}
	for k, want := range plainKeys {
		if out[k] != want {
			t.Errorf("out[%s] = %q, want unredacted %q", k, out[k], want)
		}
	}
}

func TestRedactEnv_Nil(t *testing.T) {
	if RedactEnv(nil) != nil {
		t.Error("RedactEnv(nil) should return nil")
	}
}
