package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/agentsprobe"
	"github.com/aceteam-ai/citadel-cli/internal/harnessseat"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	errSeatCLIOwner    = errors.New("seat_local_owner_unresolved")
	errSeatCLIOutput   = errors.New("seat_output_failed")
	errSeatCLICommand  = errors.New("seat_subcommand_required")
	errSeatCLITerminal = errors.New("seat_human_terminal_required")
)

type seatCLIOwner struct{ Name, Home string }
type seatCLIOwnerResolver func() (seatCLIOwner, error)

// Resolve the actual unprivileged caller through the existing owner resolver's
// explicit passwd tier, not HOME, SUDO_USER, node credentials or a remote name.
func resolveSeatCLIOwner() (seatCLIOwner, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return seatCLIOwner{}, harnessseat.ErrStoreUnsupported
	}
	return resolveSeatCLIProcessOwner(os.Getuid(), os.Geteuid(), user.LookupId, agentsprobe.ResolveTargetUserForNode)
}

func resolveSeatCLIProcessOwner(uid, euid int, lookup func(string) (*user.User, error), resolve func(agentsprobe.ResolveInputs) (agentsprobe.Target, error)) (seatCLIOwner, error) {
	if uid <= 0 || uid != euid {
		return seatCLIOwner{}, errSeatCLIOwner
	}
	caller, err := lookup(strconv.Itoa(uid))
	if err != nil || caller == nil || caller.Username == "" {
		return seatCLIOwner{}, errSeatCLIOwner
	}
	target, err := resolve(agentsprobe.ResolveInputs{ConfiguredUser: caller.Username, ProcessUID: uid})
	if err != nil || target.UID != uid || target.Username == "" {
		return seatCLIOwner{}, errSeatCLIOwner
	}
	return seatCLIOwner{Name: target.Username, Home: target.HomeDir}, nil
}

// Only these offline metadata leaves skip ordinary node startup. Keep profile
// admission before this exception; the HPC positive list is unchanged.
func isHarnessSeatCLI(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "citadel harness seat init", "citadel harness seat list", "citadel harness seat add", "citadel harness seat enable", "citadel harness seat disable", "citadel harness seat remove":
		return true
	}
	return false
}

func withSeatCLIRegistry(cmd *cobra.Command, dir string, resolveOwner seatCLIOwnerResolver, action func(*harnessseat.SaltStore, func(string) (string, error)) error) (result error) {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return harnessseat.ErrPrivateStore
	}
	owner, err := resolveOwner()
	if err != nil {
		return err
	}
	if owner.Name == "" || !filepath.IsAbs(owner.Home) || filepath.Clean(owner.Home) != owner.Home || filepath.Dir(owner.Home) == owner.Home {
		return errSeatCLIOwner
	}
	// A private leaf alone does not protect an explicit path through a
	// writable/symlinked ancestor. Restrict this human CLI to the resolved
	// owner's home and reuse the component-by-component no-adoption check.
	pin, err := harnessseat.PrepareLocalSeatDirectory(harnessseat.Identity{ID: "registry", Harness: harnessseat.Codex, Owner: owner.Name, HomeDir: owner.Home, ConfigDir: dir}, false)
	if err != nil {
		return harnessseat.ErrPrivateStore
	}
	defer func() {
		if err := pin.Close(); result == nil {
			result = err
		}
	}()
	store, err := harnessseat.OpenSaltStore(dir)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := store.Close()
		if result == nil {
			result = closeErr
		}
	}()
	if err := pin.Check(); err != nil {
		return harnessseat.ErrPrivateStore
	}
	resolve := func(name string) (string, error) {
		if name != owner.Name {
			return "", errSeatCLIOwner
		}
		return owner.Home, nil
	}
	return action(store, resolve)
}

// This local display intentionally uses the human's local seat slug, not the
// node identity needed by the separate server projection. Paths, OS usernames
// and account digests are excluded. Registry status is never called a probe.
type seatCLIListRow struct {
	ID          string                `json:"id"`
	Harness     harnessseat.Harness   `json:"harness"`
	Label       string                `json:"label"`
	Entitlement string                `json:"entitlement"`
	PlanType    string                `json:"plan_type,omitempty"`
	Models      []string              `json:"models"`
	DataCeiling string                `json:"data_ceiling"`
	Enabled     bool                  `json:"enabled"`
	Auth        harnessseat.AuthState `json:"auth"`
}

func newHarnessSeatRoot(resolveOwner seatCLIOwnerResolver) *cobra.Command {
	return newHarnessSeatRootWithTerminal(resolveOwner, func(cmd *cobra.Command) bool {
		input, ok := cmd.InOrStdin().(*os.File)
		return ok && term.IsTerminal(int(input.Fd()))
	})
}

// Terminal presence is a local interaction guard, not remote authorization or
// proof of native credential isolation. No vendor process is ever executed.
func newHarnessSeatRootWithTerminal(resolveOwner seatCLIOwnerResolver, terminal func(*cobra.Command) bool) *cobra.Command {
	harness := &cobra.Command{Use: "harness", Short: "Manage local harness metadata"}
	seat := &cobra.Command{Use: "seat", Short: "Manage an explicit private local seat registry", Long: "Owner-only local registry metadata. Add and enable require a human terminal. Login instructions are printed only, never executed. No credential-file reader, status probe, job, remote enable or process launch is used. Probe and default-home adoption remain incomplete."}
	seat.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return errSeatCLICommand
		}
		return cobra.NoArgs(cmd, args)
	}
	seat.RunE = func(cmd *cobra.Command, args []string) error { return errSeatCLICommand }
	harness.AddCommand(seat)
	var dir string
	seat.PersistentFlags().StringVar(&dir, "registry-dir", "", "Required existing absolute 0700 registry directory under the caller's home; never defaults to node state")
	initialize := &cobra.Command{Use: "init", Short: "Explicitly initialize an empty registry without adopting vendor homes", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		return withSeatCLIRegistry(cmd, dir, resolveOwner, func(store *harnessseat.SaltStore, resolve func(string) (string, error)) error {
			if _, err := store.InitializeRegistry(cmd.Context(), resolve); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Private registry initialized; no vendor homes adopted."); err != nil {
				return errSeatCLIOutput
			}
			return nil
		})
	}}
	var asJSON bool
	list := &cobra.Command{Use: "list", Short: "List saved declarations, not live login or quota status", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		return withSeatCLIRegistry(cmd, dir, resolveOwner, func(store *harnessseat.SaltStore, resolve func(string) (string, error)) error {
			snapshot, err := store.LoadRegistry(cmd.Context(), resolve)
			if err != nil {
				return err
			}
			rows := make([]seatCLIListRow, 0)
			for _, s := range snapshot.Seats() {
				rows = append(rows, seatCLIListRow{ID: s.ID, Harness: s.Harness, Label: s.Label, Entitlement: s.Entitlement, PlanType: s.PlanType, Models: append([]string{}, s.Models...), DataCeiling: s.DataCeiling, Enabled: s.Enabled, Auth: s.Auth})
			}
			if asJSON {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					StatusSource string           `json:"status_source"`
					Seats        []seatCLIListRow `json:"seats"`
				}{"registry_declaration", rows}); err != nil {
					return errSeatCLIOutput
				}
				return nil
			}
			out := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if _, err := fmt.Fprintln(out, "Saved registry declarations (not probed)\nID\tHARNESS\tLABEL\tENABLED\tSAVED AUTH"); err != nil {
				return errSeatCLIOutput
			}
			for _, row := range rows {
				if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%t\t%s\n", row.ID, row.Harness, row.Label, row.Enabled, row.Auth); err != nil {
					return errSeatCLIOutput
				}
			}
			if err := out.Flush(); err != nil {
				return errSeatCLIOutput
			}
			return nil
		})
	}}
	list.Flags().BoolVar(&asJSON, "json", false, "Output local metadata without paths or account digests")
	seat.AddCommand(initialize, list)
	var harnessName, label, configDir, entitlement, ceiling string
	var roots []string
	add := &cobra.Command{Use: "add <id>", Short: "Create a disabled local declaration and print human login instructions", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !terminal(cmd) {
			return errSeatCLITerminal
		}
		var owner seatCLIOwner
		boundOwner := func() (seatCLIOwner, error) {
			var err error
			owner, err = resolveOwner()
			return owner, err
		}
		return withSeatCLIRegistry(cmd, dir, boundOwner, func(store *harnessseat.SaltStore, resolve func(string) (string, error)) error {
			identity := harnessseat.Identity{ID: args[0], Harness: harnessseat.Harness(harnessName), Owner: owner.Name, HomeDir: owner.Home, ConfigDir: configDir}
			instruction, err := seatHumanLoginInstruction(identity, entitlement, os.Getenv("PATH"))
			if err != nil {
				return err
			}
			snapshot, err := store.LoadRegistry(cmd.Context(), resolve)
			if err != nil {
				return err
			}
			_, err = store.AddLocalSeat(cmd.Context(), snapshot, harnessseat.LocalSeatDeclaration{ID: identity.ID, Harness: identity.Harness, Owner: identity.Owner, ConfigDir: identity.ConfigDir, Label: label, Entitlement: entitlement, Roots: roots, DataCeiling: ceiling})
			if err != nil {
				return err
			}
			// Paths appear only on this local human terminal, never a server projection.
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Seat saved disabled with unknown auth. Native isolation is NOT verified.\nPrinted only, not executed; choose the declared login method in the vendor tool:\n%s\nNo credential or login output is collected. Local enable does not authorize launch.\n", instruction); err != nil {
				return errSeatCLIOutput
			}
			return nil
		})
	}}
	add.Flags().StringVar(&harnessName, "harness", "", "Required: claude, codex, copilot or muse")
	add.Flags().StringVar(&label, "label", "", "Required human-readable label")
	add.Flags().StringVar(&configDir, "config-dir", "", "Required fresh absolute configuration directory under the resolved owner's home")
	add.Flags().StringVar(&entitlement, "entitlement", "consumer", "Declared entitlement, not observed authentication or permission")
	add.Flags().StringVar(&ceiling, "data-ceiling", "public", "Declared policy selector; Muse permits public only")
	add.Flags().StringArrayVar(&roots, "root", nil, "Declared absolute workspace root; repeat for multiple roots")
	seat.AddCommand(add)
	for _, verb := range []string{"enable", "disable", "remove"} {
		verb := verb
		command := &cobra.Command{Use: verb + " <id>", Short: verb + " a declaration without opening or deleting vendor files", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if verb == "enable" && !terminal(cmd) {
				return errSeatCLITerminal
			}
			return withSeatCLIRegistry(cmd, dir, resolveOwner, func(store *harnessseat.SaltStore, resolve func(string) (string, error)) error {
				snapshot, err := store.LoadRegistry(cmd.Context(), resolve)
				if err != nil {
					return err
				}
				if verb == "disable" || verb == "enable" {
					_, err = store.SetLocalSeatEnabled(cmd.Context(), snapshot, args[0], verb == "enable")
				} else {
					_, err = store.RemoveLocalSeat(cmd.Context(), snapshot, args[0])
				}
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Registry declaration updated; vendor files untouched."); err != nil {
					return errSeatCLIOutput
				}
				return nil
			})
		}}
		seat.AddCommand(command)
	}
	return harness
}

// A printable POSIX command, not an execution API. Build from an allowlist so
// inherited API keys, provider overrides and shell startup hooks cannot appear.
// It is not a promise that a vendor's keychain/daemon honors this home.
func seatHumanLoginInstruction(identity harnessseat.Identity, entitlement, ownerPath string) (string, error) {
	environment, err := harnessseat.ChildEnvironment(identity, ownerPath, nil)
	if err != nil {
		return "", err
	}
	var command string
	switch identity.Harness {
	case harnessseat.Claude:
		if entitlement == "api_key" {
			// API-key helpers are configured by the human in the vendor's own
			// settings. Never accept a key/helper command as registry metadata.
			path, err := quoteHookArgForOS("linux", filepath.Join(identity.ConfigDir, "settings.json"))
			if err != nil {
				return "", err
			}
			return "Configure apiKeyHelper manually in " + path + "; no value is accepted or read by Citadel.", nil
		}
		command = "claude auth login"
	case harnessseat.Codex:
		command = "codex login"
		if entitlement == "api_key" {
			command += " --with-api-key"
		}
	case harnessseat.Copilot:
		command = "copilot login"
		if entitlement == "api_key" {
			command += " --with-token"
		}
	case harnessseat.Muse:
		command = "muse auth"
	default:
		return "", harnessseat.ErrInvalidIdentity
	}
	parts := []string{"env", "-i"}
	for _, entry := range environment {
		quoted, err := quoteHookArgForOS("linux", entry)
		if err != nil {
			return "", err
		}
		parts = append(parts, quoted)
	}
	return strings.Join(parts, " ") + " " + command, nil
}

func init() { rootCmd.AddCommand(newHarnessSeatRoot(resolveSeatCLIOwner)) }
