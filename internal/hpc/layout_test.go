//go:build !windows

package hpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLayout(t *testing.T) {
	base := t.TempDir()
	cases := []struct {
		name    string
		in      Inputs
		root    string
		wantErr string
	}{
		{name: "explicit state root", in: Inputs{JobID: "4242", StateRoot: base, TmpDir: "/nope"}, root: filepath.Join(base, "citadel-job-4242")},
		{name: "tmpdir fallback", in: Inputs{JobID: "7", TmpDir: base}, root: filepath.Join(base, "citadel-job-7")},
		{name: "legacy id agrees", in: Inputs{JobID: "7", LegacyJobID: "7", TmpDir: base}, root: filepath.Join(base, "citadel-job-7")},
		{name: "requeue gets a fresh root", in: Inputs{JobID: "7", RestartCount: "2", TmpDir: base}, root: filepath.Join(base, "citadel-job-7-r2")},
		{name: "zero restart count", in: Inputs{JobID: "7", RestartCount: "0", TmpDir: base}, root: filepath.Join(base, "citadel-job-7")},
		{name: "no job id", in: Inputs{TmpDir: base}, wantErr: "SLURM_JOB_ID is not set"},
		{name: "only legacy id", in: Inputs{LegacyJobID: "7", TmpDir: base}, wantErr: "ambiguous"},
		{name: "ids disagree", in: Inputs{JobID: "7", LegacyJobID: "8", TmpDir: base}, wantErr: "disagree"},
		{name: "non-numeric id", in: Inputs{JobID: "7/../../x", TmpDir: base}, wantErr: "not a numeric"},
		{name: "bad restart count", in: Inputs{JobID: "7", RestartCount: "x", TmpDir: base}, wantErr: "not a number"},
		{name: "no base", in: Inputs{JobID: "7"}, wantErr: "no job state directory"},
		{name: "relative base", in: Inputs{JobID: "7", StateRoot: "scratch"}, wantErr: "absolute"},
		{name: "dotdot base", in: Inputs{JobID: "7", StateRoot: base + "/../x"}, wantErr: "'..'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, err := ResolveLayout(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveLayout: %v", err)
			}
			if l.Root != tc.root {
				t.Fatalf("root = %q, want %q", l.Root, tc.root)
			}
			for _, d := range l.Dirs() {
				if !strings.HasPrefix(d, l.Root+string(filepath.Separator)) {
					t.Fatalf("%s is not under the job root %s", d, l.Root)
				}
			}
		})
	}
}

func TestResolveLayoutDistinctJobsNeverShareAPath(t *testing.T) {
	base := t.TempDir()
	a, err := ResolveLayout(Inputs{JobID: "100", TmpDir: base})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolveLayout(Inputs{JobID: "101", TmpDir: base})
	if err != nil {
		t.Fatal(err)
	}
	for _, da := range append(a.Dirs(), a.Root) {
		for _, db := range append(b.Dirs(), b.Root) {
			if da == db || strings.HasPrefix(da, db+"/") || strings.HasPrefix(db, da+"/") {
				t.Fatalf("job layouts overlap: %s and %s", da, db)
			}
		}
	}
}

func TestPrepareCreatesOwnerOnlyTree(t *testing.T) {
	l, err := ResolveLayout(Inputs{JobID: "9", TmpDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := Prepare(l, os.Getuid()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, d := range append([]string{l.Root}, l.Dirs()...) {
		info, err := os.Lstat(d)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %#o, want 0700", d, info.Mode().Perm())
		}
	}
	// A second Prepare (enroll, then work, in one allocation) reuses the tree.
	if err := Prepare(l, os.Getuid()); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
}

func TestPrepareRefusals(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("ownership refusals cannot be exercised as root")
	}
	t.Run("root user", func(t *testing.T) {
		l, _ := ResolveLayout(Inputs{JobID: "1", TmpDir: t.TempDir()})
		if err := Prepare(l, 0); err == nil || !strings.Contains(err.Error(), "root") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing base", func(t *testing.T) {
		l, _ := ResolveLayout(Inputs{JobID: "1", TmpDir: filepath.Join(t.TempDir(), "absent")})
		if err := Prepare(l, os.Getuid()); err == nil {
			t.Fatal("expected an error for a missing base")
		}
	})
	t.Run("world writable base without sticky bit", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Chmod(base, 0o777); err != nil {
			t.Fatal(err)
		}
		l, _ := ResolveLayout(Inputs{JobID: "1", TmpDir: base})
		if err := Prepare(l, os.Getuid()); err == nil || !strings.Contains(err.Error(), "sticky") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("world writable base with sticky bit", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Chmod(base, 0o777|os.ModeSticky); err != nil {
			t.Fatal(err)
		}
		l, _ := ResolveLayout(Inputs{JobID: "1", TmpDir: base})
		if err := Prepare(l, os.Getuid()); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	})
	t.Run("symlinked root", func(t *testing.T) {
		base := t.TempDir()
		target := t.TempDir()
		l, _ := ResolveLayout(Inputs{JobID: "1", TmpDir: base})
		if err := os.Symlink(target, l.Root); err != nil {
			t.Fatal(err)
		}
		if err := Prepare(l, os.Getuid()); err == nil || !strings.Contains(err.Error(), "not a plain directory") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("group readable existing root", func(t *testing.T) {
		base := t.TempDir()
		l, _ := ResolveLayout(Inputs{JobID: "1", TmpDir: base})
		if err := os.Mkdir(l.Root, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(l.Root, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := Prepare(l, os.Getuid()); err == nil || !strings.Contains(err.Error(), "0700") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("root owned by another uid", func(t *testing.T) {
		l, _ := ResolveLayout(Inputs{JobID: "1", TmpDir: t.TempDir()})
		if err := Prepare(l, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("err = %v", err)
		}
	})
}
