package memorygit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Helpers are Go test binaries, never shell scripts. Their only input is the
// fixed argv that production runGit supplies; the name selects test behavior.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "driver" {
		_ = os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), "started"), []byte("unexpected driver execution"), 0600)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--no-pager" {
		marker := filepath.Join(filepath.Dir(os.Args[0]), "started")
		_ = os.WriteFile(marker, []byte("started"), 0600)
		switch filepath.Base(os.Args[0]) {
		case "slow-git":
			time.Sleep(time.Minute)
		case "large-git":
			for range 100 {
				_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), MaxOutputBytes))
			}
		case "env-git":
			for _, v := range os.Environ() {
				if strings.Contains(v, "fixture-secret") {
					os.Exit(12)
				}
			}
			_, _ = os.Stdout.WriteString(strings.Repeat("a", 40) + "\n")
		case "bad-git":
			_, _ = os.Stderr.WriteString("fixture-secret /private/node/path")
			os.Exit(1)
		default:
			os.Exit(13)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixture(t *testing.T) (*Engine, string, string, string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	fixtureGit(t, git, repo, "init", "--quiet")
	if err := os.Chmod(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, git, repo, "config", "user.name", "Fixture")
	fixtureGit(t, git, repo, "config", "user.email", "fixture@example.invalid")
	path := filepath.Join(repo, "memories", "person-fixture", "_note.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, git, repo, "add", ".")
	fixtureGit(t, git, repo, "commit", "--quiet", "-m", "first | separator")
	old := strings.TrimSpace(fixtureGit(t, git, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(path, []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, git, repo, "add", ".")
	fixtureGit(t, git, repo, "commit", "--quiet", "-m", "second")
	newSHA := strings.TrimSpace(fixtureGit(t, git, repo, "rev-parse", "HEAD"))
	engine, err := New("node-fixture", repo, git, map[string]string{"person-fixture": "memories/person-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return engine, repo, old, newSHA
}

func fixtureGitCommand(git, repo string, args ...string) *exec.Cmd {
	// Automatic maintenance may detach and remove a lock while fixtureGit
	// normalizes metadata permissions. Keep fixture writes synchronous.
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0", "-C", repo}, args...)
	cmd := exec.Command(git, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + repo, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
	return cmd
}

func fixtureGit(t *testing.T, git, repo string, args ...string) string {
	t.Helper()
	cmd := fixtureGitCommand(git, repo, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git failed: %v: %s", err, out)
	}
	// Fixtures model the node-owned private repository, independent of the
	// invoking harness's permissive umask.
	if err := filepath.WalkDir(filepath.Join(repo, ".git"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()&^0022)
	}); err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestFixtureGitDoesNotStartAutomaticMaintenance(t *testing.T) {
	e, repo, _, _ := fixture(t)
	// Repository settings request maintenance. The fixture command must
	// override them, even for commits that normally spawn maintenance --auto.
	fixtureGit(t, e.gitPath, repo, "config", "maintenance.auto", "true")
	fixtureGit(t, e.gitPath, repo, "config", "gc.auto", "1")
	tracePath := filepath.Join(t.TempDir(), "git-trace.json")
	cmd := fixtureGitCommand(e.gitPath, repo, "commit", "--allow-empty", "--quiet", "-m", "trace fixture")
	cmd.Env = append(cmd.Env, "GIT_TRACE2_EVENT="+tracePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture commit: %v %s", err, out)
	}
	trace, err := os.Open(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	decoder := json.NewDecoder(trace)
	started := false
	for {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if event.Event == "start" {
			started = true
		}
		if event.Event == "child_start" {
			for _, arg := range event.Argv {
				if arg == "maintenance" || arg == "gc" {
					t.Fatalf("fixture started asynchronous Git maintenance: %v", event.Argv)
				}
			}
		}
	}
	if !started {
		t.Fatal("Git trace did not observe the fixture commit")
	}
}

func request(op Operation) Request {
	return Request{Version: 1, NodeID: "node-fixture", Scope: "person-fixture", Operation: op, Name: "_note"}
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("got %v, want code %s", err, code)
	}
}

func helper(t *testing.T, name string) (string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.Symlink(exe, path); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(dir, "started")
}

func TestReadOperationsAndNoMutation(t *testing.T) {
	e, repo, old, current := fixture(t)
	// Uncommitted content and another staged file must remain untouched.
	path := filepath.Join(repo, "memories/person-fixture/_note.md")
	if err := os.WriteFile(path, []byte("uncommitted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "unrelated"), []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, e.gitPath, repo, "add", "unrelated")
	before := fixtureGit(t, e.gitPath, repo, "status", "--porcelain=v1")
	for _, revision := range []string{current, current[:7], strings.ToUpper(current), "HEAD", "HEAD~1"} {
		r := request(Resolve)
		r.Name = ""
		r.Revision = revision
		out, err := e.Execute(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		want := current
		if revision == "HEAD~1" {
			want = old
		}
		if out.SHA != want {
			t.Fatalf("resolve: got %s want %s", out.SHA, want)
		}
	}
	r := request(History)
	r.Limit = 2
	out, err := e.Execute(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Commits) != 2 || out.Commits[0].SHA != current || out.Commits[1].SHA != old || out.Commits[1].Subject != "first | separator" {
		t.Fatalf("history: %+v", out)
	}
	r = request(Diff)
	r.From = old
	r.To = "HEAD"
	out, err = e.Execute(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if out.FromSHA != old || out.ToSHA != current || !strings.Contains(out.Diff, "-old\n+new\n") || strings.Contains(out.Diff, "unrelated") {
		t.Fatalf("diff: %+v", out)
	}
	r = request(Export)
	r.Revision = "HEAD~1"
	out, err = e.Execute(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	content, err := base64.StdEncoding.DecodeString(out.ContentBase64)
	if err != nil || string(content) != "old\n" || out.Bytes != 4 {
		t.Fatalf("export: %+v %v", out, err)
	}
	if after := fixtureGit(t, e.gitPath, repo, "status", "--porcelain=v1"); after != before {
		t.Fatalf("repository changed: %q -> %q", before, after)
	}
}

func TestHostileOperandsRejectedBeforeExec(t *testing.T) {
	e, _, _, _ := fixture(t)
	e.gitPath, _ = helper(t, "env-git")
	marker := filepath.Join(filepath.Dir(e.gitPath), "started")
	for _, hostile := range []string{"HEAD~100", "HEAD^", "HEAD:path", "HEAD\n", "--help", "a;id", "$(id)", "../outside", "a\x00b", "refs/heads/main", "", strings.Repeat("f", 41)} {
		t.Run("revision", func(t *testing.T) {
			r := request(Export)
			r.Revision = hostile
			_, err := e.Execute(context.Background(), r)
			assertCode(t, err, "invalid_request")
		})
	}
	for _, name := range []string{"../outside", "a/b", ".git", "note.md", "a\n", "-option", strings.Repeat("a", 253), "é"} {
		r := request(History)
		r.Limit = 2
		r.Name = name
		_, err := e.Execute(context.Background(), r)
		assertCode(t, err, "invalid_request")
	}
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Version = 2 }, func(r *Request) { r.NodeID = "other-node" }, func(r *Request) { r.Scope = "other-scope" },
		func(r *Request) { r.Operation = "rollback" }, func(r *Request) { r.Limit = 101 }, func(r *Request) { r.From = "HEAD" },
	} {
		r := request(History)
		r.Limit = 2
		mutate(&r)
		if _, err := e.Execute(context.Background(), r); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid input started child")
	}
}

func TestOutputLimitAndCancellation(t *testing.T) {
	e, _, _, _ := fixture(t)
	r := request(Resolve)
	r.Name = ""
	r.Revision = "HEAD"
	e.gitPath, _ = helper(t, "large-git")
	start := time.Now()
	_, err := e.Execute(context.Background(), r)
	assertCode(t, err, "output_limit")
	if time.Since(start) > 3*time.Second {
		t.Fatal("output limit did not stop child promptly")
	}
	var marker string
	e.gitPath, marker = helper(t, "slow-git")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = e.Execute(ctx, r)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("cancellation fixture never started child")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("child ignored cancellation")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = e.Execute(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel: %v", err)
	}
}

func TestChildEnvironmentAndSanitizedErrors(t *testing.T) {
	e, _, _, _ := fixture(t)
	r := request(Resolve)
	r.Name = ""
	r.Revision = "HEAD"
	for _, name := range []string{"GIT_DIR", "GIT_CONFIG_COUNT", "LD_PRELOAD", "AWS_SECRET_ACCESS_KEY", "HOME"} {
		t.Setenv(name, "fixture-secret")
	}
	e.gitPath, _ = helper(t, "env-git")
	if _, err := e.Execute(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	e.gitPath, _ = helper(t, "bad-git")
	_, err := e.Execute(context.Background(), r)
	assertCode(t, err, "git_failed")
	if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "/private/") {
		t.Fatal("stderr leaked")
	}
}

func TestRepositoryAndBlobConfinement(t *testing.T) {
	for _, mutation := range []string{"git-symlink", "alternate", "commondir", "world-writable", "metadata-symlink", "metadata-hardlink"} {
		t.Run(mutation, func(t *testing.T) {
			e, repo, _, _ := fixture(t)
			switch mutation {
			case "git-symlink":
				outside := filepath.Join(t.TempDir(), "git")
				if err := os.Rename(filepath.Join(repo, ".git"), outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(repo, ".git")); err != nil {
					t.Fatal(err)
				}
			case "alternate":
				if err := os.WriteFile(filepath.Join(repo, ".git/objects/info/alternates"), []byte("/outside"), 0600); err != nil {
					t.Fatal(err)
				}
			case "commondir":
				if err := os.WriteFile(filepath.Join(repo, ".git/commondir"), []byte("/outside"), 0600); err != nil {
					t.Fatal(err)
				}
			case "world-writable":
				if err := os.Chmod(filepath.Join(repo, ".git"), 0777); err != nil {
					t.Fatal(err)
				}
			case "metadata-symlink":
				if err := os.Symlink("/outside", filepath.Join(repo, ".git/objects/link")); err != nil {
					t.Fatal(err)
				}
			case "metadata-hardlink":
				if err := os.Link(filepath.Join(repo, ".git/HEAD"), filepath.Join(t.TempDir(), "HEAD")); err != nil {
					t.Fatal(err)
				}
			}
			r := request(Export)
			r.Revision = "HEAD"
			_, err := e.Execute(context.Background(), r)
			assertCode(t, err, "repository_unavailable")
		})
	}
	e, repo, _, _ := fixture(t)
	path := filepath.Join(repo, "memories/person-fixture/link.md")
	if err := os.Symlink("/outside/secret", path); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, e.gitPath, repo, "add", ".")
	fixtureGit(t, e.gitPath, repo, "commit", "--quiet", "-m", "link")
	r := request(Export)
	r.Name = "link"
	r.Revision = "HEAD"
	_, err := e.Execute(context.Background(), r)
	assertCode(t, err, "file_unavailable")
	r.Name = "missing"
	_, err = e.Execute(context.Background(), r)
	assertCode(t, err, "file_unavailable")
}

func TestConfigurationCannotGrantPathsOutsideMemory(t *testing.T) {
	for _, path := range []string{"../outside", "/outside", "memories/../outside", "memories/a/b", "memories/a\n"} {
		_, err := New("node-fixture", t.TempDir(), "/usr/bin/git", map[string]string{"scope": path})
		assertCode(t, err, "invalid_configuration")
	}
	mapping := map[string]string{"scope": "memories/allowed"}
	e, err := New("node-fixture", t.TempDir(), "/usr/bin/git", mapping)
	if err != nil {
		t.Fatal(err)
	}
	mapping["scope"] = "memories/other"
	if e.scopes["scope"] != "memories/allowed" {
		t.Fatal("configuration alias changed scope")
	}
}

func TestPinnedRepositorySurvivesDirectoryReplacement(t *testing.T) {
	e, repo, _, current := fixture(t)
	fd, err := openRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, "saved-git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := runGit(context.Background(), e.gitPath, fd, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(out)) != current {
		t.Fatalf("pinned repository changed: %q %v", out, err)
	}
}

func TestDiffNeverRunsConfiguredExternalDriver(t *testing.T) {
	e, repo, old, current := fixture(t)
	driver, marker := helper(t, "driver")
	fixtureGit(t, e.gitPath, repo, "config", "diff.memory.command", driver)
	fixtureGit(t, e.gitPath, repo, "config", "diff.memory.textconv", driver)
	fixtureGit(t, e.gitPath, repo, "config", "core.fsmonitor", driver)
	fixtureGit(t, e.gitPath, repo, "config", "core.pager", driver)
	if err := os.WriteFile(filepath.Join(repo, ".git/info/attributes"), []byte("*.md diff=memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := request(Diff)
	r.From = old
	r.To = current
	out, err := e.Execute(context.Background(), r)
	if err != nil || !strings.Contains(out.Diff, "-old\n+new\n") {
		t.Fatalf("fixed diff failed: %+v %v", out, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("external Git driver ran")
	}
}

func TestExportBytesAndBoundaryLimit(t *testing.T) {
	e, repo, _, _ := fixture(t)
	path := filepath.Join(repo, "memories/person-fixture/_note.md")
	for _, size := range []int{MaxOutputBytes, MaxOutputBytes + 1} {
		content := bytes.Repeat([]byte{0, 255, 1, 128}, (size+3)/4)[:size]
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		fixtureGit(t, e.gitPath, repo, "add", ".")
		fixtureGit(t, e.gitPath, repo, "commit", "--quiet", "-m", "bytes")
		r := request(Export)
		r.Revision = "HEAD"
		out, err := e.Execute(context.Background(), r)
		if size > MaxOutputBytes {
			assertCode(t, err, "output_limit")
			if out.ContentBase64 != "" {
				t.Fatal("partial export escaped limit")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.StdEncoding.DecodeString(out.ContentBase64)
		if err != nil || !bytes.Equal(content, decoded) || out.Bytes != size {
			t.Fatal("export did not preserve bytes")
		}
	}
}
