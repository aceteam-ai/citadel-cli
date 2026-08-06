package compose

import (
	"strings"
	"testing"
)

// livePSOutput is a real `docker compose -f vllm.yml ps --format json` capture
// from a citadel node, trimmed to the fields the operator surfaces read and with
// image names genericized.
//
// Note what it contains: nine running containers, and NOT ONE of them is vllm.
// That is the whole of citadel-cli#692. Because citadel passes no `-p` (#528) and
// every service compose file shares the default project, `ps` scoped to vllm.yml
// returns the entire project, so a caller that reads the first record concludes
// vllm is running when vllm has no container at all.
const livePSOutput = `
{"ID":"61bfa011e53f","Name":"citadel-bonsai","Image":"citadel-bonsai:local","Service":"bonsai","State":"running","Status":"Up 10 hours","Ports":"0.0.0.0:8210->8080/tcp"}
{"ID":"97d7fac7c972","Name":"citadel-gotenberg","Image":"gotenberg:8","Service":"gotenberg","State":"running","Status":"Up 10 hours (healthy)","Ports":"127.0.0.1:8209->3000/tcp"}
{"ID":"53b6c466f956","Name":"citadel-kokoro","Image":"kokoro-service:latest","Service":"kokoro","State":"running","Status":"Up 10 hours (healthy)","Ports":"127.0.0.1:8211->8080/tcp"}
{"ID":"3e8aac0c4d1b","Name":"citadel-meeting","Image":"meeting-service:latest","Service":"meeting","State":"running","Status":"Up 10 hours (healthy)","Ports":"127.0.0.1:8207->8102/tcp"}
{"ID":"94b5ccaf96eb","Name":"citadel-tei","Image":"text-embeddings-inference:cpu-1.6","Service":"tei","State":"running","Status":"Up 10 hours","Ports":"127.0.0.1:8102->80/tcp"}
{"ID":"f85ff6db1979","Name":"citadel-transcribe","Image":"whisper-service:latest","Service":"transcribe","State":"running","Status":"Up 10 hours","Ports":"0.0.0.0:8101->8101/tcp"}
{"ID":"88aee0273bb4","Name":"citadel-unlimited-ocr","Image":"vllm-openai:unlimited-ocr","Service":"unlimited-ocr","State":"running","Status":"Up 10 hours","Ports":"0.0.0.0:8213->8000/tcp"}
{"ID":"2b1f20435fb5","Name":"services-bridge-1","Image":"whatsapp-bridge:latest","Service":"bridge","State":"running","Status":"Up 10 hours (healthy)","Ports":"0.0.0.0:8083->8080/tcp"}
{"ID":"cd3c5f192d7e","Name":"services-db-1","Image":"postgres:16-alpine","Service":"db","State":"running","Status":"Up 10 hours (healthy)","Ports":"5432/tcp"}
`

func never() bool { return false }
func always() bool { return true }

// TestResolveServiceStateContainerBacked covers the ordinary case: a service
// whose own container is in the project-wide output and is up.
func TestResolveServiceStateContainerBacked(t *testing.T) {
	got := ResolveServiceState([]byte(livePSOutput), map[string]bool{"kokoro": true}, never)
	if !got.Running || got.State != StateRunning {
		t.Fatalf("kokoro: got %+v, want running", got)
	}
	if got.Native {
		t.Error("kokoro is container-backed; Native must be false")
	}
	if got.Container == nil || got.Container.Name != "citadel-kokoro" {
		t.Fatalf("kokoro resolved to the wrong container: %+v", got.Container)
	}
	if got.Container.Status != "Up 10 hours (healthy)" {
		t.Errorf("uptime source lost: Status = %q", got.Container.Status)
	}
}

// TestResolveServiceStateMultiContainerService covers whatsapp-bridge, whose
// compose file declares two services (`bridge`, `db`) and pins no container_name
// -- so its containers are named services-bridge-1 / services-db-1. This is the
// case that makes declared-service-name matching necessary: a lookup for a
// container literally named "citadel-whatsapp-bridge" finds nothing and would
// report a running service as stopped.
func TestResolveServiceStateMultiContainerService(t *testing.T) {
	declared := map[string]bool{"bridge": true, "db": true}
	got := ResolveServiceState([]byte(livePSOutput), declared, never)
	if !got.Running {
		t.Fatalf("whatsapp-bridge: got %+v, want running", got)
	}
	if got.Container == nil || !strings.HasPrefix(got.Container.Name, "services-") {
		t.Fatalf("resolved to the wrong container: %+v", got.Container)
	}
}

// TestResolveServiceStateNative is the guard against fixing #692 in the opposite
// direction. Ollama runs as a native systemd service on some nodes
// (`/usr/local/bin/ollama serve`, port 11434) and has no container at all, so
// "no container" must not be read as "not running": the native serving probe
// decides.
func TestResolveServiceStateNative(t *testing.T) {
	got := ResolveServiceState([]byte(livePSOutput), map[string]bool{"ollama": true}, always)
	if !got.Running || got.State != StateRunning {
		t.Fatalf("native ollama: got %+v, want running", got)
	}
	if !got.Native {
		t.Error("native ollama must be flagged Native")
	}
	if got.Container != nil {
		t.Errorf("native service must not claim a container: %+v", got.Container)
	}
}

// TestResolveServiceStateAbsent is the #692 regression test: vllm is declared,
// has no container, and is not serving natively. The project-wide output is full
// of OTHER services' running containers, and none of them may promote vllm to
// running.
func TestResolveServiceStateAbsent(t *testing.T) {
	got := ResolveServiceState([]byte(livePSOutput), map[string]bool{"vllm": true}, never)
	if got.Running || got.State != StateStopped {
		t.Fatalf("absent vllm: got %+v, want stopped (#692)", got)
	}
	if got.Container != nil {
		t.Errorf("absent service must not claim a container: %+v", got.Container)
	}
}

// TestResolveServiceStateNilProbe: a caller that supplies no native probe still
// gets a correct container-only answer.
func TestResolveServiceStateNilProbe(t *testing.T) {
	if got := ResolveServiceState([]byte(livePSOutput), map[string]bool{"vllm": true}, nil); got.Running {
		t.Errorf("nil probe: got %+v, want stopped", got)
	}
	if got := ResolveServiceState([]byte(livePSOutput), map[string]bool{"tei": true}, nil); !got.Running {
		t.Errorf("nil probe: got %+v, want running", got)
	}
}

// TestResolveServiceStateExited keeps the crash-loop signal readable: a declared
// container that is not up is reported with its real state, not swallowed.
func TestResolveServiceStateExited(t *testing.T) {
	out := `{"ID":"aaa","Name":"citadel-vllm","Service":"vllm","State":"exited","Status":"Exited (1) 2 minutes ago"}`
	got := ResolveServiceState([]byte(out), map[string]bool{"vllm": true}, never)
	if got.Running {
		t.Fatalf("exited vllm reported running: %+v", got)
	}
	if got.State != StateStopped {
		t.Errorf("State = %q, want %q", got.State, StateStopped)
	}
	if got.Container == nil || got.Container.State != "exited" {
		t.Errorf("raw container state lost: %+v", got.Container)
	}
}

// TestResolveServiceStatePrefersRunning: when a multi-service compose file has
// one container up and one down, the running one is selected so the uptime and
// state read off it are the live ones.
func TestResolveServiceStatePrefersRunning(t *testing.T) {
	out := `{"ID":"a","Name":"services-db-1","Service":"db","State":"exited","Status":"Exited (0)"}
{"ID":"b","Name":"services-bridge-1","Service":"bridge","State":"running","Status":"Up 3 hours"}`
	got := ResolveServiceState([]byte(out), map[string]bool{"bridge": true, "db": true}, never)
	if !got.Running || got.Container == nil || got.Container.ID != "b" {
		t.Fatalf("got %+v, want the running container", got)
	}
}

// TestResolveServiceStateFailsOpen: when the compose file could not be parsed
// for its declared services, fall back to the previous project-wide reading
// rather than to "stopped". A false running is the status quo; a false stopped
// would be a new defect in the more damaging direction.
func TestResolveServiceStateFailsOpen(t *testing.T) {
	got := ResolveServiceState([]byte(livePSOutput), nil, never)
	if !got.Running {
		t.Fatalf("unknown declared services must fail open to the old behavior, got %+v", got)
	}
}

func TestResolveServiceStateEmptyOutput(t *testing.T) {
	if got := ResolveServiceState(nil, map[string]bool{"vllm": true}, never); got.Running {
		t.Errorf("empty ps output: got %+v, want stopped", got)
	}
	if got := ResolveServiceState([]byte("   \n"), nil, never); got.Running {
		t.Errorf("blank ps output: got %+v, want stopped", got)
	}
}

// TestParsePSArrayForm: newer docker compose emits a single JSON array for
// `--format json` instead of one object per line. Both must parse, or the fix
// silently degrades to "no containers" (i.e. everything stopped) on those hosts.
func TestParsePSArrayForm(t *testing.T) {
	arr := `[{"ID":"a","Service":"vllm","State":"running"},{"ID":"b","Service":"tei","State":"exited"}]`
	got := ParsePS([]byte(arr))
	if len(got) != 2 || got[0].Service != "vllm" || got[1].Service != "tei" {
		t.Fatalf("array form parsed as %+v", got)
	}
	st := ResolveServiceState([]byte(arr), map[string]bool{"vllm": true}, never)
	if !st.Running {
		t.Errorf("array form: got %+v, want running", st)
	}
}

func TestParsePSMalformed(t *testing.T) {
	if got := ParsePS([]byte("not json at all")); len(got) != 0 {
		t.Errorf("malformed output parsed as %+v", got)
	}
	if got := ParsePS([]byte("[{bad}]")); len(got) != 0 {
		t.Errorf("malformed array parsed as %+v", got)
	}
}

func TestDeclaredServicesFromYAML(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "single service with pinned container name",
			yaml: "services:\n  vllm:\n    image: vllm/vllm-openai\n    container_name: citadel-vllm\n",
			want: []string{"vllm"},
		},
		{
			name: "two services, compose-default container names",
			yaml: "services:\n  bridge:\n    image: bridge:latest\n  db:\n    image: postgres:16-alpine\n",
			want: []string{"bridge", "db"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeclaredServicesFromYAML([]byte(tt.yaml))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for _, name := range tt.want {
				if !got[name] {
					t.Errorf("missing declared service %q in %v", name, got)
				}
			}
		})
	}
}

// TestDeclaredServicesUnparseable pins the fail-open contract at the source: an
// unreadable or malformed compose file yields nil, which FilterPS treats as
// "unknown" rather than "declares nothing".
func TestDeclaredServicesUnparseable(t *testing.T) {
	if got := DeclaredServicesFromYAML([]byte(":::not yaml:::")); got != nil {
		t.Errorf("malformed yaml: got %v, want nil", got)
	}
	if got := DeclaredServicesFromYAML([]byte("version: '3'\n")); got != nil {
		t.Errorf("no services block: got %v, want nil", got)
	}
	if got := DeclaredServices("/nonexistent/path/to/compose.yml"); got != nil {
		t.Errorf("missing file: got %v, want nil", got)
	}
}
