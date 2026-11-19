package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/containersession"
	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/spf13/cobra"
)

// Container declared sessions (#1533, #1165 S1). containersession.Runner owns
// the engine calls, credentials and wake rule; this file only wires the node's
// real runtime, binary, key file and tmpfs, and the three operator verbs.

const (
	containerSessionKeyFile   = "session-vault.key"
	containerSessionCredsDir  = "session-credentials"
	containerSessionReadyWait = 90 * time.Second
	containerSessionPoll      = 500 * time.Millisecond
	maxCredentialStdin        = 64 << 10
)

// containerSessionKeySealer opens the node-key compartment on first use, so a
// wake of a session with no credentials never reads or creates the key file.
type containerSessionKeySealer struct {
	path   string
	create bool
	open   func(string, bool) (*nodevault.Session, error)
	sess   *nodevault.Session
}

func (s *containerSessionKeySealer) session() (*nodevault.Session, error) {
	if s.sess != nil {
		return s.sess, nil
	}
	open := s.open
	if open == nil {
		open = nodevault.OpenKeyCompartment
	}
	sess, err := open(s.path, s.create)
	if err != nil {
		return nil, err
	}
	s.sess = sess
	return sess, nil
}

func (s *containerSessionKeySealer) Seal(c string, p []byte) ([]byte, error) {
	sess, err := s.session()
	if err != nil {
		return nil, err
	}
	return sess.Seal(c, p)
}

func (s *containerSessionKeySealer) Unseal(c string, p []byte) ([]byte, error) {
	sess, err := s.session()
	if err != nil {
		return nil, err
	}
	return sess.Unseal(c, p)
}

// containerSessionRunBase is a memory-backed directory the invoking user owns.
// containersession.IsTmpfs proves it before any secret is written.
func containerSessionRunBase() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" && filepath.IsAbs(dir) {
		return dir
	}
	return "/run/user/" + strconv.Itoa(os.Getuid())
}

func newContainerSessionRunner(nodeDir string, createKey bool) (*containersession.Runner, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("container sessions run as the node's unprivileged user, not root")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, errors.New("cannot identify Citadel executable")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	runner := &containersession.Runner{
		RT:      catalog.SelectContainerRuntime(),
		NodeDir: nodeDir, RunBase: containerSessionRunBase(), IsTmpfs: containersession.IsTmpfs,
		Binary: exe, UID: os.Getuid(), GID: os.Getgid(),
		Log: func(format string, args ...any) { Debug(format, args...) },
		ClearStop: func(ctx context.Context, dir, name string) error {
			return declaredsession.ClearContainerStopMarker(ctx, dir, name)
		},
		Creds: &containersession.SealedFileStore{
			Dir:    filepath.Join(nodeDir, containerSessionCredsDir),
			Sealer: &containerSessionKeySealer{path: filepath.Join(nodeDir, containerSessionKeyFile), create: createKey},
		},
	}
	runner.WaitReady = func(ctx context.Context, name, incarnation string) error {
		return waitContainerHolderReady(ctx, runner, nodeDir, name, incarnation)
	}
	return runner, nil
}

// containerHolderVerifier is the engine-side proof DialContainerHolder needs.
func containerHolderVerifier(runner *containersession.Runner) declaredsession.ContainerVerifier {
	return func(ctx context.Context, name, incarnation string) error {
		obs, err := runner.Observe(ctx, name)
		if err != nil {
			return err
		}
		if !obs.Exists || !obs.HasRecord || obs.State != "running" || obs.Record.Incarnation != incarnation {
			return errors.New("container does not match the saved holder incarnation")
		}
		return nil
	}
}

func waitContainerHolderReady(ctx context.Context, runner *containersession.Runner, nodeDir, name, incarnation string) error {
	store, err := declaredsession.OpenStore(nodeDir)
	if err != nil {
		return err
	}
	defer store.Close()
	deadline := time.NewTimer(containerSessionReadyWait)
	defer deadline.Stop()
	tick := time.NewTicker(containerSessionPoll)
	defer tick.Stop()
	for {
		if client, err := dialContainerHolderOnce(ctx, store, runner, name); err == nil {
			ok := client.Hello().Incarnation == incarnation
			_ = client.Close()
			if ok {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("holder did not answer in time")
		case <-tick.C:
		}
	}
}

func dialContainerHolderOnce(ctx context.Context, store *declaredsession.Store, runner *containersession.Runner, name string) (*declaredsession.Client, error) {
	session, err := store.OpenSession(name, false)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	return declaredsession.DialContainerHolder(ctx, session, containerHolderVerifier(runner))
}

func containerDeclaration(nodeDir, name string) (declaredsession.Declaration, error) {
	snapshot, err := declaredsession.LoadConfig(nodeDir)
	if err != nil {
		return declaredsession.Declaration{}, err
	}
	d, ok := snapshot.Lookup(name)
	if !ok || !d.IsContainer() {
		return declaredsession.Declaration{}, fmt.Errorf("%q is not a declared container session", name)
	}
	return d, nil
}

func newContainerSessionCommands(nodeDir *string) []*cobra.Command {
	var out []*cobra.Command
	for _, action := range []string{"wake", "sleep"} {
		action := action
		out = append(out, &cobra.Command{
			Use:   action + " <name>",
			Short: strings.ToUpper(action[:1]) + action[1:] + " a declared container session",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, names []string) error {
				if declaredsession.ValidateName(names[0]) != nil {
					return declaredsession.ErrConfig
				}
				selected, err := selectedDeclaredNodeDir(cmd, *nodeDir)
				if err != nil {
					return err
				}
				d, err := containerDeclaration(selected, names[0])
				if err != nil {
					return err
				}
				runner, err := newContainerSessionRunner(selected, false)
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
				defer cancel()
				if action == "sleep" {
					return runner.Sleep(ctx, d)
				}
				rec, err := runner.Wake(ctx, d)
				if errors.Is(err, containersession.ErrAlreadyRunning) {
					fmt.Fprintf(cmd.OutOrStdout(), "%s is already running\n", d.Name)
					return nil
				}
				if err == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "%s awake (volume %s)\n", rec.Name, rec.Volume)
				}
				return err
			},
		})
	}
	credential := &cobra.Command{Use: "credential", Short: "Manage a container session's sealed credentials"}
	credential.AddCommand(&cobra.Command{
		Use:   "set <name> <credential>",
		Short: "Seal one declared credential for a session, reading the value from stdin",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selected, err := selectedDeclaredNodeDir(cmd, *nodeDir)
			if err != nil {
				return err
			}
			value, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxCredentialStdin+1))
			if err != nil || len(value) > maxCredentialStdin {
				return errors.New("credential value unreadable or too large")
			}
			runner, err := newContainerSessionRunner(selected, true)
			if err != nil {
				return err
			}
			d, err := containerDeclaration(selected, args[0])
			if err != nil {
				return err
			}
			return setContainerCredential(runner.Creds, d, args[1], value)
		},
	})
	return append(out, credential)
}

// setContainerCredential seals one declared credential, merging into whatever
// bundle already exists. A trailing newline from a shell pipe is dropped for an
// env credential only; a file credential is stored byte for byte.
func setContainerCredential(store containersession.CredentialStore, d declaredsession.Declaration, credential string, value []byte) error {
	var spec *declaredsession.CredentialSpec
	for i := range d.Credentials {
		if d.Credentials[i].Name == credential {
			spec = &d.Credentials[i]
		}
	}
	if spec == nil {
		return fmt.Errorf("%q does not declare a credential named %q", d.Name, credential)
	}
	if spec.Env != "" {
		value = []byte(strings.TrimRight(string(value), "\r\n"))
	}
	if len(value) == 0 {
		return errors.New("empty credential refused")
	}
	bundle, err := store.Load(d.Name)
	if errors.Is(err, containersession.ErrNoCredentials) {
		bundle, err = containersession.Bundle{}, nil
	}
	if err != nil {
		return err
	}
	bundle[credential] = value
	return store.Save(d.Name, bundle)
}

// reconcileContainerSessions wakes declared container sessions the engine shows
// were interrupted (a reboot killed them). Called once at worker start by the
// single-instance worker; it never blocks startup and never fails the worker.
func reconcileContainerSessions(nodeDir string) {
	snapshot, err := declaredsession.LoadConfig(nodeDir)
	if err != nil {
		return // no declarations (or not Linux): nothing to wake
	}
	has := false
	for _, d := range snapshot.Sessions {
		has = has || d.IsContainer()
	}
	if !has {
		return
	}
	runner, err := newContainerSessionRunner(nodeDir, false)
	if err != nil {
		Debug("container sessions: reconcile skipped: %v", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		for _, o := range runner.Reconcile(ctx, snapshot) {
			if o.Err != nil {
				Log("container session %s: %s: %v", o.Decision.Name, o.Decision.Reason, o.Err)
			} else if o.Decision.Wake {
				Log("container session %s woken (%s)", o.Decision.Name, o.Decision.Reason)
			}
		}
	}()
}
