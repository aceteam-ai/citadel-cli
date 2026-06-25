package catalog

import (
	"reflect"
	"testing"
)

func TestIsChannel(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{"stable", true},
		{"latest", true},
		{"STABLE", true},
		{" latest ", true},
		{"v1.2.3", false},
		{"^1.2", false},
		{"main", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsChannel(tt.ref); got != tt.want {
			t.Errorf("IsChannel(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}

func TestIsVersionConstraint(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		// Channels.
		{"stable", true},
		{"latest", true},
		// Constraints.
		{"^1.2", true},
		{"~1.2.3", true},
		{">=1.0 <2.0", true},
		{"1.2.3", true},
		{"v1.2.3", true},
		{">=1.0.0", true},
		// Exact branches / non-constraints.
		{"main", false},
		{"dev", false},
		{"feature-x", false},
		{"", false},
		// A full SHA is an exact pin, never a constraint.
		{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", false},
		// An abbreviated SHA (7-40 hex) is also an exact pin.
		{"abc1234", false},
	}
	for _, tt := range tests {
		if got := IsVersionConstraint(tt.ref); got != tt.want {
			t.Errorf("IsVersionConstraint(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}

func TestResolveVersion(t *testing.T) {
	tags := []string{
		"v1.0.0", "v1.1.0", "v1.2.0", "v1.2.1",
		"v2.0.0-rc.1", "v2.0.0", "v2.1.0",
		"nightly", // non-semver, ignored
	}
	tests := []struct {
		name    string
		ref     string
		tags    []string
		want    string
		wantErr bool
	}{
		{name: "caret minor", ref: "^1.2", tags: tags, want: "v1.2.1"},
		{name: "caret major picks highest in 1.x", ref: "^1.0", tags: tags, want: "v1.2.1"},
		{name: "tilde patch", ref: "~1.2.0", tags: tags, want: "v1.2.1"},
		{name: "range", ref: ">=1.0 <2.0", tags: tags, want: "v1.2.1"},
		{name: "exact existing", ref: "1.2.0", tags: tags, want: "v1.2.0"},
		{name: "exact with v", ref: "v1.1.0", tags: tags, want: "v1.1.0"},
		{name: "stable skips prerelease", ref: "stable", tags: tags, want: "v2.1.0"},
		{name: "latest includes prerelease consideration", ref: "latest", tags: tags, want: "v2.1.0"},
		{
			name: "latest picks prerelease when highest",
			ref:  "latest",
			tags: []string{"v1.0.0", "v2.0.0-rc.1"},
			want: "v2.0.0-rc.1",
		},
		{
			name: "stable skips to lower stable",
			ref:  "stable",
			tags: []string{"v1.0.0", "v2.0.0-rc.1"},
			want: "v1.0.0",
		},
		{name: "no match constraint", ref: "^9.0", tags: tags, wantErr: true},
		{name: "no semver tags", ref: "^1.0", tags: []string{"nightly", "edge"}, wantErr: true},
		{name: "bare tags without v prefix", ref: "^1.0", tags: []string{"1.0.0", "1.4.2"}, want: "1.4.2"},
		{
			name:    "stable with only prereleases errors",
			ref:     "stable",
			tags:    []string{"v1.0.0-alpha", "v1.0.0-beta"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveVersion(tt.ref, tt.tags)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveVersion(%q) = %q, want error", tt.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveVersion(%q) unexpected error: %v", tt.ref, err)
			}
			if got != tt.want {
				t.Errorf("ResolveVersion(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestParseLsRemoteTags(t *testing.T) {
	out := "6333b7\trefs/tags/1.0.0\n" +
		"513f3d\trefs/tags/1.0.1\n" +
		"0b2b2f\trefs/tags/v1.1.1\n" +
		"8d0431\trefs/tags/v1.1.1^{}\n" + // dereference -> same tag, dedup
		"77e38e\trefs/tags/v1.2.0\n" +
		"deadbe\trefs/heads/main\n" + // not a tag, skipped
		"\n"
	got := parseLsRemoteTags(out)
	want := []string{"1.0.0", "1.0.1", "v1.1.1", "v1.2.0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLsRemoteTags() = %v, want %v", got, want)
	}
}
