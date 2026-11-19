package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/containersession"
	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

const containerSessionFixture = `sessions:
  - name: lane1
    harness: claude-code
    command: [claude]
    cwd: /home/agent/work/repo
    runtime: {kind: container, image: ghcr.io/aceteam-ai/session:1}
    credentials:
      - {name: oauth, path: /home/agent/.claude/.credentials.json}
      - {name: gh, env: GH_TOKEN}
`

func containerSessionDecl(t *testing.T) declaredsession.Declaration {
	t.Helper()
	snap, err := declaredsession.DecodeConfig([]byte(containerSessionFixture))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := snap.Lookup("lane1")
	return d
}

func TestSetContainerCredentialMergesAndTrimsEnvOnly(t *testing.T) {
	dir := t.TempDir()
	sess, err := nodevault.OpenKeyCompartment(filepath.Join(dir, "k"), true)
	if err != nil {
		t.Fatal(err)
	}
	store := &containersession.SealedFileStore{Dir: filepath.Join(dir, "c"), Sealer: sess}
	d := containerSessionDecl(t)
	if err := setContainerCredential(store, d, "gh", []byte("tok\n")); err != nil {
		t.Fatal(err)
	}
	if err := setContainerCredential(store, d, "oauth", []byte("{\"a\":1}\n")); err != nil {
		t.Fatal(err)
	}
	b, err := store.Load("lane1")
	if err != nil || string(b["gh"]) != "tok" || string(b["oauth"]) != "{\"a\":1}\n" {
		t.Fatalf("bundle = %q, %v", b, err)
	}
	if err := setContainerCredential(store, d, "undeclared", []byte("x")); err == nil {
		t.Error("undeclared credential accepted")
	}
	if err := setContainerCredential(store, d, "gh", []byte("\n")); err == nil {
		t.Error("empty credential accepted")
	}
}

func TestKeySealerIsLazy(t *testing.T) {
	opened := 0
	s := &containerSessionKeySealer{path: "/nope", open: func(string, bool) (*nodevault.Session, error) {
		opened++
		return nil, errors.New("no key")
	}}
	if opened != 0 {
		t.Fatal("opened at construction")
	}
	if _, err := s.Seal("c", []byte("x")); err == nil || opened != 1 {
		t.Fatalf("seal err=%v opened=%d", err, opened)
	}
}

func TestSessionHostContainerFlagRoutesToContainerHolder(t *testing.T) {
	var got declaredsession.ContainerHolderOptions
	old := runContainerHolderFn
	runContainerHolderFn = func(_ context.Context, o declaredsession.ContainerHolderOptions) error {
		got = o
		return declaredsession.ErrHolderTerminated
	}
	t.Cleanup(func() { runContainerHolderFn = old })
	hostCalled := false
	cmd := newDeclaredHostCommandWith(func(context.Context, declaredsession.RunHolderOptions) error {
		hostCalled = true
		return nil
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"lane1", "--container", "--node-config-dir", "/run/session", "--launch-id", strings.Repeat("a", 32)})
	err := cmd.Execute()
	if !errors.Is(err, declaredsession.ErrHolderTerminated) || hostCalled {
		t.Fatalf("err=%v hostCalled=%v", err, hostCalled)
	}
	if got.Name != "lane1" || got.NodeDir != "/run/session" || got.Incarnation != strings.Repeat("a", 32) {
		t.Fatalf("container holder options = %+v", got)
	}
}
