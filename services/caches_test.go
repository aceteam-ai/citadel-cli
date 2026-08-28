package services

import (
	"strings"
	"testing"
)

// TestEngineCacheDirsCoverServiceMap pins that EngineCacheDirs and ServiceMap
// enumerate exactly the same engines (citadel #906 / #682 P1). Neither
// direction is safe to skip: a ServiceMap entry with no cache-dir mapping
// means some future caller has to reinvent (or hardcode) its cache path --
// the exact class of bug this table exists to prevent -- and a cache-dir
// entry naming no embedded compose is a stale or typo'd key nothing can ever
// exercise.
func TestEngineCacheDirsCoverServiceMap(t *testing.T) {
	for name := range ServiceMap {
		if _, ok := EngineCacheDirs[name]; !ok {
			t.Errorf("ServiceMap engine %q has no EngineCacheDirs entry", name)
		}
	}
	for name := range EngineCacheDirs {
		if _, ok := ServiceMap[name]; !ok {
			t.Errorf("EngineCacheDirs entry %q names no embedded compose in ServiceMap", name)
		}
	}
}

// TestEngineCacheDirsMatchComposeMounts is the table's actual honesty check,
// exactly what docs/design-cache-ownership.md's P1 scope asks for: every
// entry's Dir must appear as a real ~/citadel-cache/<Dir> volume mount in
// that engine's OWN embedded compose file, string-matched against the actual
// embedded YAML rather than hand-copied. This is what makes it mechanically
// impossible for the table to silently drift from what the compose files
// actually do -- the exact failure mode behind citadel #906 (llamacpp's
// MODEL_CACHE_PULL writing into ~/citadel-cache/huggingface, a directory
// services/compose/llamacpp.yml never mounts).
func TestEngineCacheDirsMatchComposeMounts(t *testing.T) {
	for engine, cache := range EngineCacheDirs {
		compose, ok := ServiceMap[engine]
		if !ok {
			t.Errorf("engine %q has a cache-dir entry but no embedded compose", engine)
			continue
		}
		// The trailing ":" is deliberate: it requires the ACTUAL volume mount
		// syntax ("~/citadel-cache/<dir>:<container-path>"), not merely a
		// mention of the directory name anywhere in the file (e.g. bonsai.yml
		// has a prose comment naming the very same path a few lines above its
		// real `volumes:` entry -- a bare Contains without the colon would
		// pass on that comment alone even if the actual mount line drifted).
		want := "~/citadel-cache/" + cache.Dir + ":"
		if !strings.Contains(compose, want) {
			t.Errorf("engine %q: compose does not mount %q (EngineCacheDirs says Dir=%q) -- "+
				"the table and the compose file have diverged", engine, want, cache.Dir)
		}
	}
}

// TestLlamaCppAndBonsaiHaveSeparateGGUFDirs pins the CacheFamilyGGUFDir
// design choice explicitly: llamacpp and bonsai are both raw-GGUF engines but
// must NOT share a directory, or an eviction/disk-report for one could not
// tell its files apart from the other's.
func TestLlamaCppAndBonsaiHaveSeparateGGUFDirs(t *testing.T) {
	llamacpp, ok := EngineCacheDirs["llamacpp"]
	if !ok {
		t.Fatal("llamacpp missing from EngineCacheDirs")
	}
	bonsai, ok := EngineCacheDirs["bonsai"]
	if !ok {
		t.Fatal("bonsai missing from EngineCacheDirs")
	}
	if llamacpp.Family != CacheFamilyGGUFDir || bonsai.Family != CacheFamilyGGUFDir {
		t.Errorf("expected both llamacpp and bonsai to be CacheFamilyGGUFDir, got llamacpp=%q bonsai=%q", llamacpp.Family, bonsai.Family)
	}
	if llamacpp.Dir == bonsai.Dir {
		t.Errorf("llamacpp and bonsai must not share a cache directory, both resolved to %q", llamacpp.Dir)
	}
}
