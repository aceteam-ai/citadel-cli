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
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/agentsprobe"
	"github.com/aceteam-ai/citadel-cli/internal/harnessseat"
	"github.com/spf13/cobra"
)

var (
	errSeatCLIOwner  = errors.New("seat_local_owner_unresolved")
	errSeatCLIOutput = errors.New("seat_output_failed")
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
	case "citadel harness seat init", "citadel harness seat list", "citadel harness seat disable", "citadel harness seat remove":
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
	harness := &cobra.Command{Use: "harness", Short: "Manage local harness metadata"}
	seat := &cobra.Command{Use: "seat", Short: "Inspect or restrict an explicit private seat registry", Long: "Owner-only local registry metadata. No vendor credential files, login, status probe, job, remote enable or process launch is used. Incomplete seat-registry build: add/enable/probe and default-home adoption are not exposed yet."}
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
	for _, verb := range []string{"disable", "remove"} {
		verb := verb
		command := &cobra.Command{Use: verb + " <id>", Short: verb + " a declaration without opening or deleting vendor files", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return withSeatCLIRegistry(cmd, dir, resolveOwner, func(store *harnessseat.SaltStore, resolve func(string) (string, error)) error {
				snapshot, err := store.LoadRegistry(cmd.Context(), resolve)
				if err != nil {
					return err
				}
				if verb == "disable" {
					_, err = store.SetLocalSeatEnabled(cmd.Context(), snapshot, args[0], false)
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

func init() { rootCmd.AddCommand(newHarnessSeatRoot(resolveSeatCLIOwner)) }
