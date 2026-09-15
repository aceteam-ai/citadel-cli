package services

import "testing"

func TestResolveBindAddr(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"all", AllInterfacesBindAddr, false},
		{"ALL", AllInterfacesBindAddr, false},
		{"0.0.0.0", AllInterfacesBindAddr, false},
		{"*", AllInterfacesBindAddr, false},
		{"any", AllInterfacesBindAddr, false},
		{"loopback", LoopbackBindAddr, false},
		{"localhost", LoopbackBindAddr, false},
		{"127.0.0.1", LoopbackBindAddr, false},
		{"lan", "", true},
		{"192.168.1.5", "", true},
		{"yes", "", true},
	}
	for _, tt := range tests {
		got, err := ResolveBindAddr(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ResolveBindAddr(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ResolveBindAddr(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBindEnv(t *testing.T) {
	// Not hatch-capable: never inject.
	if entry, inject, err := BindEnv("kokoro", "all"); err != nil || inject || entry != "" {
		t.Errorf("BindEnv(kokoro, all) = (%q,%v,%v), want (\"\",false,nil)", entry, inject, err)
	}
	// Hatch-capable, unset: inject nothing (compose default wins).
	if entry, inject, err := BindEnv("vllm", ""); err != nil || inject || entry != "" {
		t.Errorf("BindEnv(vllm, \"\") = (%q,%v,%v), want (\"\",false,nil)", entry, inject, err)
	}
	// Hatch-capable, all: inject 0.0.0.0.
	entry, inject, err := BindEnv("vllm", "all")
	if err != nil || !inject || entry != EnvVLLMBind+"=0.0.0.0" {
		t.Errorf("BindEnv(vllm, all) = (%q,%v,%v), want (%q,true,nil)", entry, inject, err, EnvVLLMBind+"=0.0.0.0")
	}
	// Hatch-capable, loopback: inject 127.0.0.1 (explicit tighten, e.g. ollama).
	entry, inject, err = BindEnv("ollama", "loopback")
	if err != nil || !inject || entry != EnvOllamaBind+"=127.0.0.1" {
		t.Errorf("BindEnv(ollama, loopback) = (%q,%v,%v), want (%q,true,nil)", entry, inject, err, EnvOllamaBind+"=127.0.0.1")
	}
	// Unrecognized: error.
	if _, _, err := BindEnv("vllm", "nonsense"); err == nil {
		t.Error("BindEnv(vllm, nonsense) should error")
	}
}

func TestSplitComposePortFields(t *testing.T) {
	tests := []struct {
		spec string
		want []string
	}{
		{"11434:11434", []string{"11434", "11434"}},
		{"127.0.0.1:8201:8000", []string{"127.0.0.1", "8201", "8000"}},
		{"${CITADEL_DIFFUSERS_HOST_PORT:?msg}:7860", []string{"${CITADEL_DIFFUSERS_HOST_PORT:?msg}", "7860"}},
		{"127.0.0.1:${CITADEL_TTS_HOST_PORT}:8080", []string{"127.0.0.1", "${CITADEL_TTS_HOST_PORT}", "8080"}},
		{"${CITADEL_VLLM_BIND:-127.0.0.1}:${CITADEL_VLLM_HOST_PORT}:8000", []string{"${CITADEL_VLLM_BIND:-127.0.0.1}", "${CITADEL_VLLM_HOST_PORT}", "8000"}},
		{"${CITADEL_OLLAMA_BIND:-0.0.0.0}:11434:11434", []string{"${CITADEL_OLLAMA_BIND:-0.0.0.0}", "11434", "11434"}},
		{"8000", []string{"8000"}},
	}
	for _, tt := range tests {
		got := splitComposePortFields(tt.spec)
		if len(got) != len(tt.want) {
			t.Errorf("splitComposePortFields(%q) = %v, want %v", tt.spec, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitComposePortFields(%q)[%d] = %q, want %q", tt.spec, i, got[i], tt.want[i])
			}
		}
	}
}

func TestComposePortHostToken(t *testing.T) {
	tests := []struct {
		spec string
		want string
	}{
		{"11434:11434", "11434"},
		{"127.0.0.1:8201:8000", "8201"},
		{"${CITADEL_DIFFUSERS_HOST_PORT:?msg}:7860", "${CITADEL_DIFFUSERS_HOST_PORT:?msg}"},
		{"127.0.0.1:${CITADEL_TTS_HOST_PORT}:8080", "${CITADEL_TTS_HOST_PORT}"},
		{"${CITADEL_VLLM_BIND:-127.0.0.1}:${CITADEL_VLLM_HOST_PORT}:8000", "${CITADEL_VLLM_HOST_PORT}"},
		{"${CITADEL_OLLAMA_BIND:-0.0.0.0}:11434:11434", "11434"},
		{"8000", ""},
	}
	for _, tt := range tests {
		if got := ComposePortHostToken(tt.spec); got != tt.want {
			t.Errorf("ComposePortHostToken(%q) = %q, want %q", tt.spec, got, tt.want)
		}
	}
}

func TestResolvePortSpecBind(t *testing.T) {
	tests := []struct {
		name        string
		spec        string
		env         map[string]string
		wantBind    string
		wantPublish bool
	}{
		{"literal loopback", "127.0.0.1:8201:8000", nil, "127.0.0.1", true},
		{"literal wildcard", "0.0.0.0:8201:8000", nil, "0.0.0.0", true},
		{"no ip is wildcard", "11434:11434", nil, "", true},
		{"container only", "8000", nil, "", false},
		{"bind default loopback", "${CITADEL_VLLM_BIND:-127.0.0.1}:${CITADEL_VLLM_HOST_PORT}:8000", nil, "127.0.0.1", true},
		{"bind default all", "${CITADEL_OLLAMA_BIND:-0.0.0.0}:11434:11434", nil, "0.0.0.0", true},
		{"bind env overrides to all", "${CITADEL_VLLM_BIND:-127.0.0.1}:${CITADEL_VLLM_HOST_PORT}:8000", map[string]string{EnvVLLMBind: "0.0.0.0"}, "0.0.0.0", true},
		{"bind env overrides to loopback", "${CITADEL_OLLAMA_BIND:-0.0.0.0}:11434:11434", map[string]string{EnvOllamaBind: "127.0.0.1"}, "127.0.0.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bind, has := ResolvePortSpecBind(tt.spec, tt.env)
			if bind != tt.wantBind || has != tt.wantPublish {
				t.Errorf("ResolvePortSpecBind(%q) = (%q,%v), want (%q,%v)", tt.spec, bind, has, tt.wantBind, tt.wantPublish)
			}
		})
	}
}

func TestComposePublishesLoopbackOnly(t *testing.T) {
	const loopbackForm = "services:\n  vllm:\n    ports:\n      - \"${CITADEL_VLLM_BIND:-127.0.0.1}:${CITADEL_VLLM_HOST_PORT}:8000\"\n"
	const ollamaForm = "services:\n  ollama:\n    ports:\n      - \"${CITADEL_OLLAMA_BIND:-0.0.0.0}:11434:11434\"\n"
	const noPublish = "services:\n  x:\n    ports:\n      - \"8000\"\n"

	// Default env: the 5-engine form is loopback-only, ollama is not.
	if lb, has := ComposePublishesLoopbackOnly(loopbackForm, nil); !lb || !has {
		t.Errorf("loopback form default: got (lb=%v,has=%v), want (true,true)", lb, has)
	}
	if lb, has := ComposePublishesLoopbackOnly(ollamaForm, nil); lb || !has {
		t.Errorf("ollama form default: got (lb=%v,has=%v), want (false,true)", lb, has)
	}
	// bind:all env flips the 5-engine form to non-loopback (the drift loop guard).
	if lb, has := ComposePublishesLoopbackOnly(loopbackForm, map[string]string{EnvVLLMBind: "0.0.0.0"}); lb || !has {
		t.Errorf("loopback form with bind=all env: got (lb=%v,has=%v), want (false,true)", lb, has)
	}
	// bind:loopback env tightens ollama to loopback.
	if lb, has := ComposePublishesLoopbackOnly(ollamaForm, map[string]string{EnvOllamaBind: "127.0.0.1"}); !lb || !has {
		t.Errorf("ollama form with bind=loopback env: got (lb=%v,has=%v), want (true,true)", lb, has)
	}
	// No host publish: not loopback-only, no publish.
	if lb, has := ComposePublishesLoopbackOnly(noPublish, nil); lb || has {
		t.Errorf("no publish: got (lb=%v,has=%v), want (false,false)", lb, has)
	}
}
