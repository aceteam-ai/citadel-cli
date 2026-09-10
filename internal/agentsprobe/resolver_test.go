package agentsprobe

import (
	"os/user"
	"testing"
)

// fakeLookups builds injectable passwd lookups for the resolver core.
func fakeLookups(byName map[string]*user.User, byUID map[int]*user.User) (func(string) (*user.User, error), func(int) (*user.User, error)) {
	name := func(n string) (*user.User, error) {
		if u, ok := byName[n]; ok {
			return u, nil
		}
		return nil, user.UnknownUserError(n)
	}
	uid := func(id int) (*user.User, error) {
		if u, ok := byUID[id]; ok {
			return u, nil
		}
		return nil, user.UnknownUserIdError(id)
	}
	return name, uid
}

func TestResolveTargetUserForNode_TierPriority(t *testing.T) {
	const procPath = "/usr/bin:/bin"

	alice := &user.User{Username: "alice", Uid: "1000", Gid: "1000", HomeDir: "/home/alice"}
	bob := &user.User{Username: "bob", Uid: "1001", Gid: "1001", HomeDir: "/home/bob"}
	carol := &user.User{Username: "carol", Uid: "1002", Gid: "1002", HomeDir: "/home/carol"}

	name, uid := fakeLookups(
		map[string]*user.User{"alice": alice, "carol": carol},
		map[int]*user.User{1000: alice, 1001: bob},
	)

	baseDeps := resolveDeps{
		lookupName:  name,
		lookupUID:   uid,
		processHome: func() (string, error) { return "/root", nil },
		processPath: procPath,
	}

	t.Run("configured user wins over everything", func(t *testing.T) {
		d := baseDeps
		// A node-dir owner is ALSO present; config must still win.
		d.statOwner = func(string) (int, bool) { return 1001, true }
		got, err := resolveTargetUserForNode(ResolveInputs{
			ConfiguredUser: "alice",
			StateDir:       "/home/bob/citadel-node/network",
			SudoUser:       "carol",
			ProcessUID:     0,
		}, d)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Signal != SignalConfig || got.Username != "alice" || got.UID != 1000 {
			t.Fatalf("config tier: got %+v", got)
		}
	})

	t.Run("configured user missing from passwd is an error, not a fallthrough", func(t *testing.T) {
		d := baseDeps
		d.statOwner = func(string) (int, bool) { return 1001, true } // would resolve bob
		_, err := resolveTargetUserForNode(ResolveInputs{
			ConfiguredUser: "ghost",
			StateDir:       "/x",
			ProcessUID:     0,
		}, d)
		if err == nil {
			t.Fatal("expected an error for an unresolvable configured user, got nil")
		}
	})

	t.Run("node-dir owner uid resolves to that user (home from passwd, not the path)", func(t *testing.T) {
		d := baseDeps
		d.statOwner = func(dir string) (int, bool) {
			if dir == "/etc/citadel/node/network" {
				return 1001, true // bob owns it, though the dir is outside his home
			}
			return 0, false
		}
		got, err := resolveTargetUserForNode(ResolveInputs{
			StateDir:      "/etc/citadel/node/network",
			NodeConfigDir: "/etc/citadel/node",
			SudoUser:      "carol", // lower tier, must not win
			ProcessUID:    0,
		}, d)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Signal != SignalNodeDirOwner || got.Username != "bob" || got.HomeDir != "/home/bob" {
			t.Fatalf("node-dir-owner tier: got %+v", got)
		}
	})

	t.Run("falls through to SUDO_USER when no config and dir unstat-able", func(t *testing.T) {
		d := baseDeps
		d.statOwner = func(string) (int, bool) { return 0, false }
		got, err := resolveTargetUserForNode(ResolveInputs{
			StateDir:   "/x",
			SudoUser:   "carol",
			ProcessUID: 0,
		}, d)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Signal != SignalSudoUser || got.Username != "carol" {
			t.Fatalf("sudo-user tier: got %+v", got)
		}
	})

	t.Run("falls through to process user when nothing else resolves", func(t *testing.T) {
		d := baseDeps
		d.statOwner = func(string) (int, bool) { return 0, false }
		got, err := resolveTargetUserForNode(ResolveInputs{
			StateDir:   "/x",
			SudoUser:   "root", // treated as unset
			ProcessUID: 1000,
		}, d)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Signal != SignalProcess || got.HomeDir != "/root" || got.UID != 1000 {
			t.Fatalf("process tier: got %+v", got)
		}
		// Username/GID best-effort from passwd (uid 1000 -> alice).
		if got.Username != "alice" || got.GID != 1000 {
			t.Fatalf("process tier best-effort passwd: got %+v", got)
		}
	})

	t.Run("node-dir owner uid with no passwd entry falls through, not error", func(t *testing.T) {
		d := baseDeps
		d.statOwner = func(string) (int, bool) { return 4242, true } // no passwd entry
		got, err := resolveTargetUserForNode(ResolveInputs{
			StateDir:   "/x",
			SudoUser:   "carol",
			ProcessUID: 0,
		}, d)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Signal != SignalSudoUser {
			t.Fatalf("expected fallthrough to sudo-user, got %+v", got)
		}
	})
}

func TestTargetProbeOptions_DropDecision(t *testing.T) {
	target := Target{Username: "alice", UID: 1000, GID: 1000, HomeDir: "/home/alice", PathEnv: "/home/alice/bin"}

	t.Run("root worker drops to a different non-root target", func(t *testing.T) {
		opts := target.probeOptions(0)
		if opts.DropTo == nil {
			t.Fatal("expected DropTo to be set when root worker targets a non-root user")
		}
		if opts.DropTo.UID != 1000 || opts.DropTo.GID != 1000 || opts.DropTo.Username != "alice" {
			t.Fatalf("DropTo = %+v", opts.DropTo)
		}
		if opts.HomeDir != "/home/alice" {
			t.Fatalf("HomeDir = %q", opts.HomeDir)
		}
	})

	t.Run("non-root worker never drops (no privilege)", func(t *testing.T) {
		if opts := target.probeOptions(1000); opts.DropTo != nil {
			t.Fatalf("non-root worker must not drop; got %+v", opts.DropTo)
		}
	})

	t.Run("root worker targeting root does not drop", func(t *testing.T) {
		rootTarget := Target{Username: "root", UID: 0, GID: 0, HomeDir: "/root"}
		if opts := rootTarget.probeOptions(0); opts.DropTo != nil {
			t.Fatalf("root->root must not drop; got %+v", opts.DropTo)
		}
	})
}

func TestUserPathEnv_ExtendedStaticGlobs(t *testing.T) {
	home := t.TempDir()
	got := userPathEnv(home, "/usr/bin")
	// The static user-local extensions must all appear (the design 2c list).
	for _, want := range []string{".npm-global/bin", ".local/bin", ".volta/bin", ".bun/bin", ".cargo/bin", "share/pnpm"} {
		if !containsSub(got, want) {
			t.Errorf("PathEnv %q missing %q", got, want)
		}
	}
	if !containsSub(got, "/usr/bin") {
		t.Errorf("PathEnv %q dropped the process PATH", got)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
