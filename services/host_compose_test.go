package services

import (
	"os"
	"testing"
)

func TestComposeForHostSelectsOnlySupportedLinuxCPUEngines(t *testing.T) {
	for _, name := range []string{"ollama", "llamacpp"} {
		cpu, err := os.ReadFile("compose/" + name + ".darwin.yml")
		if err != nil {
			t.Fatal(err)
		}
		for _, count := range []int{-1, 0, 1, 4} {
			probes := 0
			got, ok := ComposeForHost(name, "linux", func() int { probes++; return count })
			want := string(cpu)
			if count > 0 {
				want = ServiceMap[name]
			}
			if !ok || got != want || probes != 1 {
				t.Fatalf("%s count %d: wrong template or probes (%d)", name, count, probes)
			}
		}
	}
	noProbe := func() int { t.Fatal("unexpected GPU probe"); return 0 }
	for name, want := range ServiceMap {
		for _, goos := range []string{"darwin", "windows", "linux"} {
			if goos == "linux" && HasCPUComposeVariant(name) {
				continue
			}
			got, ok := ComposeForHost(name, goos, noProbe)
			if !ok || got != want {
				t.Fatalf("%s/%s changed static template", goos, name)
			}
		}
	}
	if _, ok := ComposeForHost("unknown", "linux", noProbe); ok {
		t.Fatal("unknown service accepted")
	}
}

func TestComposeMapForHostSamplesOnceAndDoesNotMutateStaticMap(t *testing.T) {
	probes := 0
	selected := ComposeMapForHost("linux", func() int { probes++; return 0 })
	if probes != 1 || len(selected) != len(ServiceMap) {
		t.Fatalf("wrong map/sample count: %d", probes)
	}
	for _, name := range []string{"ollama", "llamacpp"} {
		want, _ := ComposeForHost(name, "linux", func() int { return 0 })
		if selected[name] != want {
			t.Fatal("map did not use CPU template")
		}
	}
	original := ServiceMap["ollama"]
	selected["ollama"] = "changed"
	if ServiceMap["ollama"] != original {
		t.Fatal("static map mutated")
	}
	probes = 0
	selected = ComposeMapForHost("linux", func() int { probes++; return 1 })
	for name, want := range ServiceMap {
		if selected[name] != want {
			t.Fatalf("GPU map changed %s", name)
		}
	}
	if probes != 1 {
		t.Fatalf("GPU map sampled %d times", probes)
	}
	ComposeMapForHost("windows", func() int { t.Fatal("non-Linux map probed host"); return 0 })
}
