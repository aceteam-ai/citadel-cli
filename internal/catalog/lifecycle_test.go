package catalog

import (
	"reflect"
	"testing"
)

func TestCompareCommits(t *testing.T) {
	tests := []struct {
		name           string
		locked, resolv string
		want           UpdateDecision
	}{
		{"same", "abc123", "abc123", UpdateUnchanged},
		{"different", "abc123", "def456", UpdateChanged},
		{"empty locked", "", "def456", UpdateUnknown},
		{"empty resolved", "abc123", "", UpdateUnknown},
		{"both empty", "", "", UpdateUnknown},
	}
	for _, tt := range tests {
		if got := CompareCommits(tt.locked, tt.resolv); got != tt.want {
			t.Errorf("%s: CompareCommits(%q,%q) = %v, want %v", tt.name, tt.locked, tt.resolv, got, tt.want)
		}
	}
}

func TestGCCandidates(t *testing.T) {
	present := []string{"owner-repo@v1.0.0", "owner-repo@v1.1.0", "other-mod", ""}
	referenced := map[string]bool{"owner-repo@v1.1.0": true}
	got := GCCandidates(present, referenced)
	want := []string{"other-mod", "owner-repo@v1.0.0"} // sorted, excludes referenced + empty
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GCCandidates() = %v, want %v", got, want)
	}
}

func TestGCCandidatesAllReferenced(t *testing.T) {
	present := []string{"a", "b"}
	referenced := map[string]bool{"a": true, "b": true}
	if got := GCCandidates(present, referenced); len(got) != 0 {
		t.Errorf("GCCandidates() = %v, want empty", got)
	}
}

func TestReferencedCacheDirs(t *testing.T) {
	entries := []LockEntry{
		// External github source, exact ref -> dir keyed on owner-repo@ref.
		{Name: "m1", Source: "owner/repo@v1.0.0", Ref: "v1.0.0"},
		// Constraint source resolved to a tag -> dir keyed on the resolved tag.
		{Name: "m2", Source: "owner/repo2@^1.2", Ref: "^1.2", ResolvedRef: "v1.4.0"},
		// Catalog source -> not cache-backed, skipped.
		{Name: "vllm", Source: "vllm"},
	}
	got := ReferencedCacheDirs(entries)

	// m1: owner-repo@v1.0.0
	if !got["owner-repo@v1.0.0"] {
		t.Errorf("expected owner-repo@v1.0.0 referenced, got %v", got)
	}
	// m2 must key on the RESOLVED tag (v1.4.0), not the raw constraint.
	if !got["owner-repo2@v1.4.0"] {
		t.Errorf("expected owner-repo2@v1.4.0 referenced (resolved tag), got %v", got)
	}
	if got["owner-repo2@-1.2"] || got["owner-repo2@^1.2"] {
		t.Errorf("constraint ref should not key a cache dir: %v", got)
	}
	// Catalog entry contributes nothing.
	if len(got) != 2 {
		t.Errorf("expected 2 referenced dirs, got %d: %v", len(got), got)
	}
}

func TestReferencedCacheDirsRoundTrip(t *testing.T) {
	// The dir a constraint install actually wrote == the dir gc reconstructs.
	src, err := ParseSource("owner/repo@^1.2")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate ResolveSource rewriting the ref to the resolved tag.
	src.Ref = "v1.4.0"
	wrote := sanitizeCacheName(src)

	entry := LockEntry{Name: "m", Source: "owner/repo@^1.2", Ref: "^1.2", ResolvedRef: "v1.4.0"}
	ref := ReferencedCacheDirs([]LockEntry{entry})
	if !ref[wrote] {
		t.Errorf("gc reconstruction %v does not contain the dir ResolveSource wrote: %q", ref, wrote)
	}
}

func TestIsSafeCacheName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"owner-repo@v1.0.0", true},
		{"plain", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"../escape", false},
		{"a\\b", false},
	}
	for _, tt := range tests {
		if got := isSafeCacheName(tt.name); got != tt.want {
			t.Errorf("isSafeCacheName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSourceFromLockHonorsRequestedRef(t *testing.T) {
	// Update must re-resolve the constraint (Ref="^1.2"), not pin the old tag.
	e := LockEntry{Name: "m", Source: "owner/repo@^1.2", Ref: "^1.2", ResolvedRef: "v1.3.0"}
	src, err := SourceFromLock(e)
	if err != nil {
		t.Fatal(err)
	}
	if src.Ref != "^1.2" {
		t.Errorf("SourceFromLock ref = %q, want the requested constraint ^1.2", src.Ref)
	}
}

func TestSourceAtCommit(t *testing.T) {
	e := LockEntry{Name: "m", Source: "owner/repo@^1.2", Ref: "^1.2"}
	// Valid SHA -> pinned.
	src, err := SourceAtCommit(e, "abc1234def5678")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.Ref != "abc1234def5678" {
		t.Errorf("ref = %q, want abc1234def5678", src.Ref)
	}
	// Non-SHA commit -> refuse to roll back.
	if _, err := SourceAtCommit(e, "not-a-sha-tag"); err == nil {
		t.Error("expected error rolling back to a non-SHA commit")
	}
}

func TestHasHealthProbe(t *testing.T) {
	tests := []struct {
		hc   HealthCheck
		want bool
	}{
		{HealthCheck{}, false},
		{HealthCheck{Port: 8080}, true},
		{HealthCheck{Endpoint: "/health"}, true},
		{HealthCheck{Endpoint: "  "}, false},
		{HealthCheck{Port: 8080, Endpoint: "/health"}, true},
	}
	for _, tt := range tests {
		if got := HasHealthProbe(tt.hc); got != tt.want {
			t.Errorf("HasHealthProbe(%+v) = %v, want %v", tt.hc, got, tt.want)
		}
	}
}

func TestProbeHealthNotProbeable(t *testing.T) {
	// No port/endpoint -> never a rollback trigger.
	if got := ProbeHealth(HealthCheck{}); got != ProbeNotProbeable {
		t.Errorf("ProbeHealth(empty) = %v, want ProbeNotProbeable", got)
	}
	// A closed/unused port -> connection refused -> not-probeable (not unhealthy),
	// so we never roll back spuriously when nothing is listening.
	if got := ProbeHealth(HealthCheck{Port: 1}); got == ProbeUnhealthy {
		t.Errorf("ProbeHealth(unused port) = %v, must not be ProbeUnhealthy", got)
	}
}
