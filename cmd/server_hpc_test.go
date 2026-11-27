package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestServerHPCExplicitSelectionIsOffline(t *testing.T) {
	oldAuth, oldConfig, oldDir := authServiceURL, deviceConfigForCATrustFn, nodeConfigDirFn
	t.Cleanup(func() { authServiceURL, deviceConfigForCATrustFn, nodeConfigDirFn = oldAuth, oldConfig, oldDir })
	deviceConfigForCATrustFn = func() *DeviceConfig { t.Fatal("HPC selector read host device config"); return nil }
	nodeConfigDirFn = func() string { t.Fatal("HPC selector read node directory"); return "" }
	for _, leaf := range []string{"launch", "run"} {
		for _, tc := range []struct {
			name, env, legacyEnv string
			args                 []string
			want                 string
		}{
			{name: "no explicit server"},
			{name: "flag", args: []string{"--server", "selected.example.test"}, want: "https://selected.example.test"},
			{name: "environment", env: "selected.example.test", want: "https://selected.example.test"},
			{name: "legacy flag", args: []string{"--auth-service", "selected.example.test"}, want: "https://selected.example.test"},
			{name: "legacy environment", legacyEnv: "selected.example.test", want: "https://selected.example.test"},
			{name: "flag wins environment", env: "other.example.test", args: []string{"--server", "selected.example.test"}, want: "https://selected.example.test"},
			{name: "new environment wins", env: "selected.example.test", legacyEnv: "other.example.test", want: "https://selected.example.test"},
			{name: "conflicting flags", args: []string{"--server", "selected.example.test", "--auth-service", "other.example.test"}},
			{name: "empty explicit flag", args: []string{"--server", ""}},
		} {
			t.Run(leaf+"/"+tc.name, func(t *testing.T) {
				t.Setenv("CITADEL_SERVER", tc.env)
				t.Setenv("CITADEL_AUTH_HOST", tc.legacyEnv)
				authServiceURL = defaultControlPlaneServer
				root := &cobra.Command{Use: "citadel", SilenceUsage: true, SilenceErrors: true}
				root.PersistentFlags().String("server", "", "")
				root.PersistentFlags().String("auth-service", defaultControlPlaneServer, "")
				parent := &cobra.Command{Use: "hpc"}
				parent.AddCommand(&cobra.Command{Use: leaf, RunE: func(cmd *cobra.Command, _ []string) error { return requireExplicitAuthService(cmd) }})
				root.AddCommand(parent)
				root.SetArgs(append([]string{"hpc", leaf}, tc.args...))
				err := root.Execute()
				if tc.want == "" {
					if err == nil || authServiceURL != defaultControlPlaneServer {
						t.Fatalf("missing/conflicting choice accepted: %q %v", authServiceURL, err)
					}
				} else if err != nil || authServiceURL != tc.want {
					t.Fatalf("selected %q %v, want %q", authServiceURL, err, tc.want)
				}
			})
		}
	}
}
