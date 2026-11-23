package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePerms writes body as permissions.yaml in a fresh temp dir.
func writePerms(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unreadablePerms makes permissions.yaml a directory, so reading it fails even
// for root (chmod 000 would not).
func unreadablePerms(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "permissions.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSSHDeployAllowedIn(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"missing file keeps the SSH default", t.TempDir(), true},
		{"ssh on", writePerms(t, "ssh: true\n"), true},
		{"ssh off", writePerms(t, "ssh: false\n"), false},
		{"damaged file refuses", writePerms(t, "ssh: [true\n"), false},
		{"unreadable file refuses", unreadablePerms(t), false},
	}
	for _, tc := range cases {
		if got := sshDeployAllowedIn(tc.dir); got != tc.want {
			t.Errorf("%s: sshDeployAllowedIn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFilesEnabledIn(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"missing file keeps the Files default (off)", t.TempDir(), false},
		{"files on", writePerms(t, "files: true\n"), true},
		{"files off", writePerms(t, "files: false\n"), false},
		{"damaged file refuses", writePerms(t, "files: [true\n"), false},
		{"unreadable file refuses", unreadablePerms(t), false},
	}
	for _, tc := range cases {
		if got := filesEnabledIn(tc.dir); got != tc.want {
			t.Errorf("%s: filesEnabledIn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// controlCode registers api through register with permissions read from dir,
// then serves one request on the control side with the coordinator gate passed.
func controlCode(register func(routeRegistrarSink, controlOnlyAPI, func() string), dir string) int {
	sink := &fakeSink{}
	register(sink, fakeAPI{}, func() string { return dir })
	return serve(sink.control[0], allow)
}

// The /rag control routes run the live Files gate (strict) after the
// coordinator gate.
func TestRegisterRAGControlAPIHonorsFilesPermission(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want int
	}{
		{"missing file keeps the Files default (off)", t.TempDir(), http.StatusForbidden},
		{"files off", writePerms(t, "files: false\n"), http.StatusForbidden},
		{"damaged file refuses", writePerms(t, "files: [true\n"), http.StatusForbidden},
		{"unreadable file refuses", unreadablePerms(t), http.StatusForbidden},
		{"files on", writePerms(t, "files: true\n"), http.StatusOK},
	}
	for _, tc := range cases {
		if got := controlCode(registerRAGControlAPI, tc.dir); got != tc.want {
			t.Errorf("%s: code = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The provisioning control routes run the live Provision gate (strict): a
// damaged or unreadable file refuses instead of reading as the default (on).
func TestRegisterProvisionControlAPIHonorsProvisionPermission(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want int
	}{
		{"missing file keeps the Provision default (on)", t.TempDir(), http.StatusOK},
		{"provision on", writePerms(t, "provision: true\n"), http.StatusOK},
		{"provision off", writePerms(t, "provision: false\n"), http.StatusForbidden},
		{"damaged file refuses", writePerms(t, "provision: [true\n"), http.StatusForbidden},
		{"unreadable file refuses", unreadablePerms(t), http.StatusForbidden},
	}
	for _, tc := range cases {
		if got := controlCode(registerProvisionControlAPI, tc.dir); got != tc.want {
			t.Errorf("%s: code = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestWorkControlPermissionWiring pins runWork's wiring in work.go: /rag and
// provisioning go through their permission-gated registration with the node
// permissions dir, never through an ungated registerControlOnlyAPI, and the
// status server gets the SSH permission callback.
func TestWorkControlPermissionWiring(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "work.go", nil, 0)
	if err != nil {
		t.Fatalf("parse work.go: %v", err)
	}
	gated := map[string]int{}
	sshWired := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			fn, ok := x.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch fn.Name {
			case "registerRAGControlAPI", "registerProvisionControlAPI":
				if len(x.Args) == 3 && types.ExprString(x.Args[2]) == "nodePermissionsDir" {
					gated[fn.Name]++
				} else {
					t.Errorf("%s must read nodePermissionsDir", fn.Name)
				}
			case "registerControlOnlyAPI":
				if len(x.Args) == 3 {
					api := types.ExprString(x.Args[1])
					if strings.Contains(api, "rag.") || strings.Contains(api, "provision") {
						t.Errorf("work.go registers %s without its permission gate", api)
					}
				}
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "SSHDeployAllowed" && types.ExprString(x.Value) == "nodeSSHDeployAllowed" {
				sshWired = true
			}
		}
		return true
	})
	if gated["registerRAGControlAPI"] != 1 || gated["registerProvisionControlAPI"] != 1 {
		t.Errorf("want one gated /rag and one gated provisioning registration, got %v", gated)
	}
	if !sshWired {
		t.Error("work.go must set status.ServerConfig.SSHDeployAllowed to nodeSSHDeployAllowed")
	}
}
