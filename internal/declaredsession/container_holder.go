package declaredsession

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

// ErrContainerSession is returned when a host-process control path is pointed at
// a container declaration. Those sessions are woken and slept by the container
// runner; the host manager has no scope to verify for them.
var ErrContainerSession = errors.New("declared session uses the container runtime; use citadel sessions wake or sleep")

// ErrHolderTerminated is returned by RunContainerHolder when the engine (or the
// node's sleep) signalled the holder rather than the harness ending on its own.
// The non-zero exit is the durable "this was not a deliberate operator exit"
// fact the boot reconciler reads: a container the reboot killed exits non-zero
// and is woken again, one the harness ended cleanly exits 0 and stays down.
var ErrHolderTerminated = errors.New("declared session holder terminated by signal")

// ContainerGuarantee is State.Guarantee for a holder whose custody is the
// container itself rather than a verified user scope.
const ContainerGuarantee = "container_pid1"

// containerEnv reads the process facts container custody is judged on. It is a
// seam so the checks are testable without being PID 1 in a container.
type containerEnv struct {
	pid      int
	uid, gid int
	readFile func(string) ([]byte, error)
	hostname func() (string, error)
	stat     func(string) error
}

func nativeContainerEnv() containerEnv {
	return containerEnv{
		pid: os.Getpid(), uid: os.Getuid(), gid: os.Getgid(),
		readFile: os.ReadFile, hostname: os.Hostname,
		stat: func(p string) error { _, err := os.Stat(p); return err },
	}
}

// containerCustody is the container analogue of procscope's scope proof. It
// proves only what the holder can know from inside: it is PID 1, it is not root,
// and its cgroup is not part of a citadel service unit (so a worker restart
// cannot reach it). The node verifies the container itself by label and
// engine inspect, never by D-Bus unit lookup.
type containerCustody struct {
	env      containerEnv
	identity ScopeIdentity
}

// verifyContainerSelf refuses to run unless this process is PID 1, non-root and
// outside every citadel service cgroup.
func verifyContainerSelf(env containerEnv) (*containerCustody, error) {
	if env.pid != 1 || env.uid == 0 || env.gid == 0 {
		return nil, ErrUnavailable
	}
	raw, err := env.readFile("/proc/self/cgroup")
	if err != nil || len(raw) == 0 {
		return nil, ErrUnavailable
	}
	cgroup := strings.TrimSpace(string(raw))
	if strings.Contains(cgroup, "citadel") && strings.Contains(cgroup, ".service") {
		return nil, ErrUnavailable
	}
	if strings.ContainsRune(cgroup, '\n') {
		// cgroup v1 lists many hierarchies; keep a bounded, single-line record.
		cgroup = strings.SplitN(cgroup, "\n", 2)[0]
	}
	if len(cgroup) > 4096 {
		cgroup = cgroup[:4096]
	}
	return &containerCustody{env: env, identity: ScopeIdentity{Manager: "container", ControlGroup: cgroup}}, nil
}

// CheckMembers accepts only live processes: inside a container PID namespace
// every process is the holder or its descendants, so containment itself is the
// engine's. A vanished expected process is the only failure to detect here.
func (c *containerCustody) CheckMembers(_ context.Context, allowed []procscope.ProcessIdentity) error {
	for _, p := range allowed {
		if p.PID <= 0 {
			return ErrUnavailable
		}
		if err := c.env.stat(filepath.Join("/proc", itoa(p.PID))); err != nil {
			return ErrUnavailable
		}
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ContainerHolderOptions are the in-container holder inputs. NodeDir is the
// holder's own view (ContainerNodeDir); Incarnation is minted by the node per
// wake and also lands in the container labels.
type ContainerHolderOptions struct {
	NodeDir       string
	Name          string
	Incarnation   string
	BinaryVersion string
}

// RunContainerHolder is the container entry for `citadel session-host
// --container`. It shares every behavior with RunHolder after custody
// (runHolderCore) and differs only in what it proves first: PID 1, non-root,
// outside citadel's cgroup. It returns ErrHolderTerminated when stopped by a
// signal so the container exits non-zero.
func RunContainerHolder(ctx context.Context, options ContainerHolderOptions) error {
	return runContainerHolder(ctx, options, nativeContainerEnv())
}

func runContainerHolder(ctx context.Context, options ContainerHolderOptions, env containerEnv) error {
	if ValidateName(options.Name) != nil || !filepath.IsAbs(options.NodeDir) || !validIncarnation(options.Incarnation) || len(options.BinaryVersion) > 128 {
		return ErrConfig
	}
	custody, err := verifyContainerSelf(env)
	if err != nil {
		return err
	}
	owner, err := nativeOwner()
	if err != nil {
		return err
	}
	host, err := env.hostname()
	if err != nil {
		return ErrConfig
	}
	essential := essentialEnvironment(os.LookupEnv)
	runErr := runHolderCore(ctx, RunHolderOptions{NodeDir: options.NodeDir, Name: options.Name, ExpectedIncarnation: options.Incarnation, BinaryVersion: options.BinaryVersion}, holderCore{
		owner: owner, scope: custody, identity: custody.identity, guarantee: ContainerGuarantee, container: true,
		ownerContext: OwnerContext{UID: uint32(env.uid), Name: "agent", Home: ContainerHome, Hostname: host},
		essential:    essential,
	})
	return containerExitResult(ctx, runErr)
}

// containerExitResult maps how the holder ended to its process exit: a clean
// harness exit is nil (exit 0), a signal-driven shutdown is non-zero even if
// cleanup itself succeeded.
func containerExitResult(ctx context.Context, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if ctx.Err() != nil {
		return ErrHolderTerminated
	}
	return nil
}

// ClearContainerStopMarker removes the clean-exit overlay left by a harness the
// operator ended, so an explicit wake can start the session again (OD3=B: only
// an explicit wake undoes a deliberate exit; the boot reconciler never does).
func ClearContainerStopMarker(ctx context.Context, nodeDir, name string) error {
	store, err := OpenStore(nodeDir)
	if err != nil {
		return err
	}
	defer store.Close()
	session, err := store.OpenSession(name, true)
	if err != nil {
		return err
	}
	defer session.Close()
	guard, err := session.AcquireOperation(ctx)
	if err != nil {
		return err
	}
	defer guard.Close()
	marker, present, err := session.ReadStopMarker()
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return session.ClearStopMarker(guard, marker)
}
