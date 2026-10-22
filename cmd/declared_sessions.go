package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

// Declared sessions are a node-local, same-UID boundary. These commands must
// not invoke worker startup, device login, CA installation or auto-update.
// They intentionally install their own hook instead of changing root's two
// existing startup exceptions (stream eval and session attach).
func declaredLocalOnly(*cobra.Command, []string) {}

type declaredLifecycle interface {
	Inventory(context.Context) ([]declaredsession.ManagedStatus, error)
	Status(context.Context, string) (declaredsession.ManagedStatus, error)
	Start(context.Context, string) (declaredsession.ManagedStatus, error)
	Stop(context.Context, string) (declaredsession.ManagedStatus, error)
	Restart(context.Context, string) (declaredsession.ManagedStatus, error)
}

type declaredManagerFactory func(string) (declaredLifecycle, func() error, error)

func selectedDeclaredNodeDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	// Resolve the real node directory once, at the CLI boundary. Leaf packages
	// and source fixtures never consult this global selector.
	return network.GetNodeConfigDir()
}

func openDeclaredManager(nodeDir string) (*declaredsession.Store, *declaredsession.Manager, error) {
	store, err := declaredsession.OpenStore(nodeDir)
	if err != nil {
		return nil, nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		_ = store.Close()
		return nil, nil, errors.New("cannot identify Citadel executable")
	}
	manager, err := declaredsession.NewManager(store, executable, Version)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return store, manager, nil
}

func nativeDeclaredManager(nodeDir string) (declaredLifecycle, func() error, error) {
	store, manager, err := openDeclaredManager(nodeDir)
	if err != nil {
		return nil, nil, err
	}
	return manager, store.Close, nil
}

func newDeclaredSessionsCommand(open declaredManagerFactory) *cobra.Command {
	var nodeDir string
	parent := &cobra.Command{
		Use:              "sessions",
		Short:            "Manage node-local declared harness sessions (Linux, verified user scopes)",
		Args:             cobra.NoArgs,
		PersistentPreRun: declaredLocalOnly,
		SilenceUsage:     true,
	}
	parent.PersistentFlags().StringVar(&nodeDir, "node-config-dir", "", "Existing node configuration directory (defaults to this node's configuration)")
	for _, action := range []string{"list", "status", "start", "stop", "restart"} {
		action := action
		use := action + " <name>"
		args := cobra.ExactArgs(1)
		if action == "list" {
			use, args = "list", cobra.NoArgs
		}
		child := &cobra.Command{
			Use:  use,
			Args: args,
			RunE: func(cmd *cobra.Command, names []string) error {
				if len(names) != 0 && declaredsession.ValidateName(names[0]) != nil {
					return declaredsession.ErrConfig
				}
				manager, closeStore, err := open(selectedDeclaredNodeDir(nodeDir))
				if err != nil {
					return err
				}
				// This bounds operator waits only. The independent holder never
				// inherits this cancellation or timeout as its process lifetime.
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()
				var result any
				switch action {
				case "list":
					result, err = manager.Inventory(ctx)
				case "status":
					result, err = manager.Status(ctx, names[0])
				case "start":
					result, err = manager.Start(ctx, names[0])
				case "stop":
					result, err = manager.Stop(ctx, names[0])
				case "restart":
					result, err = manager.Restart(ctx, names[0])
				}
				// Failed observations still carry useful static classifications.
				writeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result)
				return errors.Join(err, writeErr, closeStore())
			},
		}
		parent.AddCommand(child)
	}
	var events bool
	var maxBytes int
	logs := &cobra.Command{
		Use:   "logs <name>",
		Short: "Read a bounded local log tail; console output may contain secrets",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, names []string) error {
			if declaredsession.ValidateName(names[0]) != nil || maxBytes < 1 || maxBytes > 4<<20 {
				return errors.New("invalid session name or log-tail bound")
			}
			store, err := declaredsession.OpenStore(selectedDeclaredNodeDir(nodeDir))
			if err != nil {
				return err
			}
			session, err := store.OpenSession(names[0], false)
			if err != nil {
				return errors.Join(err, store.Close())
			}
			kind := declaredsession.ConsoleLog
			if events {
				kind = declaredsession.EventsLog
			}
			raw, readErr := session.ReadLogTail(kind, maxBytes)
			var writeErr error
			if readErr == nil {
				_, writeErr = cmd.OutOrStdout().Write(raw)
			}
			return errors.Join(readErr, writeErr, session.Close(), store.Close())
		},
	}
	logs.Flags().BoolVar(&events, "events", false, "Read lifecycle events instead of sensitive console bytes")
	logs.Flags().IntVar(&maxBytes, "bytes", 64<<10, "Maximum bytes from the current log (1..4194304)")
	parent.AddCommand(logs)
	return parent
}

func newDeclaredHostCommand() *cobra.Command {
	var opts declaredsession.RunHolderOptions
	command := &cobra.Command{
		Use:              "session-host <name>",
		Hidden:           true,
		Args:             cobra.ExactArgs(1),
		PersistentPreRun: declaredLocalOnly,
		SilenceUsage:     true,
		RunE: func(cmd *cobra.Command, names []string) error {
			if declaredsession.ValidateName(names[0]) != nil {
				return declaredsession.ErrConfig
			}
			opts.Name = names[0]
			opts.NodeDir = selectedDeclaredNodeDir(opts.NodeDir)
			opts.BinaryVersion = Version
			// SIGTERM is a holder lifecycle event, unlike viewer EOF. The
			// holder owns cleanup and durable publication before returning.
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, declaredTerminateSignal())
			defer cancel()
			return declaredsession.RunHolder(ctx, opts)
		},
	}
	command.Flags().StringVar(&opts.NodeDir, "node-config-dir", "", "Selected node configuration directory")
	command.Flags().StringVar(&opts.ExpectedUnit, "expected-unit", "", "Exact independently verified user scope")
	command.Flags().StringVar(&opts.ExpectedIncarnation, "launch-id", "", "Reserved launch incarnation")
	command.Flags().StringVar(&opts.OriginControlGroup, "origin-cgroup", "", "Originating caller cgroup to exclude from ownership")
	command.Flags().BoolVar(&opts.RestartCommand, "restart-command", false, "Use the declaration's explicit restart argv for this reserved incarnation")
	return command
}

func validateDeclaredAttach(cmd *cobra.Command, args []string) error {
	cmd.SilenceErrors = len(args) == 0
	if len(args) > 1 {
		return fmt.Errorf("attach accepts at most one declared session name")
	}
	declaredFlags := []string{"readonly", "take-control", "detach-keys", "node-config-dir"}
	if len(args) == 0 {
		for _, flag := range declaredFlags {
			if cmd.Flags().Changed(flag) {
				cmd.SilenceErrors = false
				return fmt.Errorf("--%s requires a declared session name", flag)
			}
		}
		return nil
	}
	if declaredsession.ValidateName(args[0]) != nil {
		return declaredsession.ErrConfig
	}
	if attachShell {
		return errors.New("--shell cannot be combined with a declared session name")
	}
	readonly, _ := cmd.Flags().GetBool("readonly")
	takeControl, _ := cmd.Flags().GetBool("take-control")
	if readonly && takeControl {
		return errors.New("--readonly cannot be combined with --take-control")
	}
	spec, _ := cmd.Flags().GetString("detach-keys")
	_, err := newDeclaredDetachDecoder(spec)
	return err
}

func init() {
	rootCmd.AddCommand(newDeclaredSessionsCommand(nativeDeclaredManager), newDeclaredHostCommand())
}
