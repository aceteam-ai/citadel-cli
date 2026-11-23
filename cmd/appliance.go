package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type applianceOptions struct {
	slots  int
	window string
}

func validateApplianceOptions(o applianceOptions) error {
	if o.slots < 1 || o.slots > 64 {
		return errors.New("browser slots must be between 1 and 64")
	}
	if o.window != "" {
		parts := strings.Split(o.window, ",")
		if len(parts) != 2 {
			return errors.New("window size must be WIDTH,HEIGHT")
		}
		for _, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 320 || n > 8192 || strconv.Itoa(n) != part {
				return errors.New("window dimensions must be whole numbers between 320 and 8192")
			}
		}
	}
	return nil
}

// readApplianceConfig never repairs malformed configuration or follows a
// symlink. Preserve unrelated device fields, including enrollment credentials.
func readApplianceConfig(dir string) (map[string]any, error) {
	path := filepath.Join(dir, "config.yaml")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("device configuration must be a readable regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("device configuration is unreadable")
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil || values == nil {
		return nil, errors.New("device configuration is malformed; refusing to rewrite it")
	}
	return values, nil
}

// applyApplianceProfile uses the existing posture, atomic config and updater
// owners. Restrict surfaces first; a later IO failure stays restrictive and is
// safely retryable. enableUpdates is injected so tests never touch live state.
func applyApplianceProfile(nodeDir, meetingDir string, o applianceOptions, enableUpdates func() error, out io.Writer) error {
	if err := validateApplianceOptions(o); err != nil {
		return err
	}
	return withNodePointerLock(filepath.Join(nodeDir, "config.yaml"), func() error {
		values, err := readApplianceConfig(nodeDir)
		if err != nil {
			return err
		}
		before, err := config.LoadPermissionsStrict(nodeDir)
		if err != nil {
			return fmt.Errorf("permissions are unreadable; refusing appliance changes: %w", err)
		}
		postureChanged := !before.CobrowsePublicOnly || before.SetupPosture != config.SetupPostureCobrowseOnly || before.Console || before.Desktop || before.Files || before.Shell || before.Services || before.SSH || before.Provision || config.LoadMeeting(meetingDir).MeetingEnabled
		if err := writeSetupPosture(nodeDir, meetingDir, "appliance"); err != nil {
			return err
		}
		if postureChanged {
			fmt.Fprintln(out, "Changed: co-browse-only permissions and public-only egress; meeting capability off")
		} else {
			fmt.Fprintln(out, "Unchanged: co-browse-only permissions and public-only egress")
		}
		if enableUpdates == nil {
			return errors.New("automatic-update writer is unavailable")
		}
		if err := enableUpdates(); err != nil {
			return fmt.Errorf("enable automatic updates: %w", err)
		}
		fmt.Fprintln(out, "Auto-update: on (existing explicit runtime opt-outs still apply)")
		changed := !reflect.DeepEqual(values["browser_slots"], o.slots) || values["browser_appliance"] != true
		values["browser_slots"], values["browser_appliance"] = o.slots, true
		if o.window != "" {
			changed = changed || values["browser_window_size"] != o.window
			values["browser_window_size"] = o.window
		}
		if changed {
			data, err := yaml.Marshal(values)
			if err != nil {
				return errors.New("could not encode device configuration")
			}
			if err := os.MkdirAll(nodeDir, 0o755); err != nil {
				return err
			}
			if err := writeConfigFileAtomic(filepath.Join(nodeDir, "config.yaml"), data); err != nil {
				return err
			}
			fmt.Fprintf(out, "Changed: appliance profile, browser slots=%d", o.slots)
		} else {
			fmt.Fprintf(out, "Unchanged: appliance profile, browser slots=%d", o.slots)
		}
		if size, ok := values["browser_window_size"].(string); ok && size != "" {
			fmt.Fprintf(out, ", window size=%s", size)
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Files and Shell: off. Existing workers read browser settings on their next start; no restart was performed.")
		return nil
	})
}

func newApplianceCommand() *cobra.Command {
	var o applianceOptions
	root := &cobra.Command{Use: "appliance", Short: "Configure a co-browse-only appliance"}
	apply := &cobra.Command{Use: "apply", Args: cobra.NoArgs, Short: "Apply the co-browse-only posture and enable automatic updates", RunE: func(cmd *cobra.Command, _ []string) error {
		return applyApplianceProfile(network.GetNodeConfigDir(), platform.ConfigDir(), o, func() error { return update.SetAutoUpdate(true) }, cmd.OutOrStdout())
	}}
	apply.Flags().IntVar(&o.slots, "browser-slots", 1, "Concurrent browser slots (1 to 64)")
	apply.Flags().StringVar(&o.window, "window-size", "", "Optional browser window size, WIDTH,HEIGHT")
	root.AddCommand(apply)
	return root
}

func init() { rootCmd.AddCommand(newApplianceCommand()) }
