package source

import "testing"

func TestResolve_Classification(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantKind Kind
		wantURL  string
		wantRef  string
		wantHost string
		wantErr  bool
	}{
		{
			name:     "bare catalog name",
			raw:      "embedding",
			wantKind: KindCatalog,
			wantURL:  "",
			wantHost: "",
		},
		{
			name:     "owner/repo no ref",
			raw:      "acme/widgets",
			wantKind: KindGitHub,
			wantURL:  "https://github.com/acme/widgets.git",
			wantHost: "github.com",
		},
		{
			name:     "owner/repo with ref",
			raw:      "acme/widgets@v1.2.0",
			wantKind: KindGitHub,
			wantURL:  "https://github.com/acme/widgets.git",
			wantRef:  "v1.2.0",
			wantHost: "github.com",
		},
		{
			name:     "https git url with .git",
			raw:      "https://git.example.com/acme/widgets.git",
			wantKind: KindGitURL,
			wantURL:  "https://git.example.com/acme/widgets.git",
			wantHost: "git.example.com",
		},
		{
			name:     "https git url with #ref",
			raw:      "https://git.example.com/acme/widgets.git#main",
			wantKind: KindGitURL,
			wantURL:  "https://git.example.com/acme/widgets.git",
			wantRef:  "main",
			wantHost: "git.example.com",
		},
		{
			name:     "scp-form git url",
			raw:      "git@github.com:acme/widgets.git",
			wantKind: KindGitURL,
			wantURL:  "git@github.com:acme/widgets.git",
			wantHost: "github.com",
		},
		{
			name:     "ssh url with userinfo",
			raw:      "ssh://git@git.example.com/acme/widgets.git",
			wantKind: KindGitURL,
			wantURL:  "ssh://git@git.example.com/acme/widgets.git",
			wantHost: "git.example.com",
		},
		{
			name:    "empty source",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "catalog name with illegal @",
			raw:     "embedding@v1",
			wantErr: true,
		},
		{
			name:    "owner/repo ref beginning with dash (injection guard)",
			raw:     "acme/widgets@-bad",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Resolve(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) = nil error, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) error: %v", tt.raw, err)
			}
			if d.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", d.Kind, tt.wantKind)
			}
			if d.CloneURL != tt.wantURL {
				t.Errorf("CloneURL = %q, want %q", d.CloneURL, tt.wantURL)
			}
			if d.Ref != tt.wantRef {
				t.Errorf("Ref = %q, want %q", d.Ref, tt.wantRef)
			}
			if d.Host != tt.wantHost {
				t.Errorf("Host = %q, want %q", d.Host, tt.wantHost)
			}
			if d.IsCatalog() != (tt.wantKind == KindCatalog) {
				t.Errorf("IsCatalog() = %v, want %v", d.IsCatalog(), tt.wantKind == KindCatalog)
			}
		})
	}
}
