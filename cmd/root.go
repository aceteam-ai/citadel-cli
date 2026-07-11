// cmd/root.go
/*
Copyright © 2025 AceTeam <dev@aceteam.ai>
*/
package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/clilog"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// getEnvOrDefault returns the value of an environment variable or a default value
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

var cfgFile string
var nexusURL string
var authServiceURL string
var debugMode bool
var daemonMode bool
var noColorGlobal bool

// noAutoUpdate, when set via the --no-auto-update persistent flag (or the
// CITADEL_NO_AUTO_UPDATE env var), opts this invocation out of every automatic
// update check and install. It exists so a hand-copied dev/test binary is not
// silently replaced by a release before it can be exercised. Explicit
// `citadel update` is unaffected.
var noAutoUpdate bool

// noMouse, when set, disables terminal mouse reporting in the control center for
// the current session, overriding the persisted preference. Users set it to keep
// their terminal's native drag-to-copy working.
var noMouse bool

// debugToStderr forces debug console output to stderr instead of stdout.
// Set by commands that use stdout as a transport (e.g., MCP).
var debugToStderr bool

// deferredUpdateNotification stores update info when TUI will handle the display
var deferredUpdateNotification string

// Log writes a message to the date-based log file and optionally to the console
// if debug mode is enabled. It always persists to file (see internal/clilog),
// but only prints to console with --debug.
func Log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)

	// Always persist to the dated, append-only log file.
	clilog.Write("", msg)

	// Print to console only if debug mode is enabled.
	if debugMode {
		timeStr := time.Now().Format("15:04:05")
		if debugToStderr {
			fmt.Fprintf(os.Stderr, "[%s] [CITADEL] %s\n", timeStr, msg)
		} else {
			fmt.Printf("[%s] [CITADEL] %s\n", timeStr, msg)
		}
	}
}

// Debug is an alias for Log (for backward compatibility).
// It always logs to file, and prints to console only if --debug is enabled.
func Debug(format string, args ...interface{}) {
	Log(format, args...)
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "citadel",
	Short: "Citadel is the agent for the AceTeam Sovereign Compute Fabric",
	Long: `Connect your hardware to AceTeam with a single command.

Just run 'citadel' — it handles login, network connection, and launches the
control center. All other subcommands are for scripting and advanced use.`,
	Version: Version,
	Run: func(cmd *cobra.Command, args []string) {
		// Default behavior: launch control center if TTY, otherwise show help
		if daemonMode {
			// TODO: Run in daemon mode (background worker)
			fmt.Println("Daemon mode not yet implemented. Use 'citadel work' for now.")
			return
		}
		if tui.IsTTY() {
			runControlCenter()
		} else {
			cmd.Help()
		}
	},
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// MCP command uses stdout as JSON-RPC transport -- any non-protocol
		// output there corrupts the stream. Redirect debug to stderr early,
		// before the first Log() call.
		if cmd.Name() == "mcp" {
			debugToStderr = true
		}

		// Handle global --no-color flag and NO_COLOR env var
		if noColorGlobal || os.Getenv("NO_COLOR") != "" {
			color.NoColor = true
		}

		// Always log the command (Log() handles console output based on --debug)
		fullCmd := "citadel"
		if cmd.Name() != "citadel" {
			fullCmd += " " + cmd.Name()
		}
		// Add flags that were set
		cmd.Flags().Visit(func(f *pflag.Flag) {
			if f.Name == "debug" {
				return // Skip the debug flag itself
			}
			if f.Value.Type() == "bool" {
				fullCmd += " --" + f.Name
			} else {
				fullCmd += " --" + f.Name + "=" + f.Value.String()
			}
		})
		if len(args) > 0 {
			fullCmd += " " + strings.Join(args, " ")
		}
		Log("command: %s", fullCmd)

		// Check for updates (skip for update/version/help commands)
		cmdName := cmd.Name()
		parentName := ""
		if cmd.Parent() != nil {
			parentName = cmd.Parent().Name()
		}
		skipUpdateCheck := cmdName == "update" || parentName == "update" ||
			cmdName == "version" || cmdName == "help" ||
			autoUpdateOptedOut()

		// Detect if we're about to launch TUI control center
		// In this case, suppress stdout printing and defer to TUI activity log
		isTUIContext := cmdName == "citadel" && len(args) == 0 && !daemonMode && tui.IsTTY()

		// MCP uses stdout as transport -- suppress update print to stdout.
		if cmdName == "mcp" {
			isTUIContext = true
		}

		if !skipUpdateCheck {
			checkForUpdateOnStartup(isTUIContext)
		}
	},
}

// autoUpdateOptedOut reports whether the user has opted out of automatic update
// checks/installs for this invocation, via the --no-auto-update persistent flag
// or a truthy CITADEL_NO_AUTO_UPDATE env var. It gates the notify-only startup
// check (skipUpdateCheck) as well as the auto-install paths.
func autoUpdateOptedOut() bool {
	return noAutoUpdate || update.IsTruthy(os.Getenv("CITADEL_NO_AUTO_UPDATE"))
}

// autoUpdateAllowed reports whether an automatic update *install* should run for
// this process: false when the user opted out (--no-auto-update /
// CITADEL_NO_AUTO_UPDATE) or when the running binary is a locally-built dev
// build rather than a real release tag. Explicit `citadel update` bypasses this.
func autoUpdateAllowed() bool {
	return update.ShouldAutoInstall(Version, noAutoUpdate, os.Getenv("CITADEL_NO_AUTO_UPDATE"))
}

// checkForUpdateOnStartup shows cached update notification and triggers background check.
// Non-blocking: shows cached result immediately, runs network check in background.
// When silent=true, the notification is stored in deferredUpdateNotification for TUI display
// instead of printing directly to stdout (which would corrupt the TUI).
func checkForUpdateOnStartup(silent bool) {
	state, err := update.LoadState()
	if err != nil {
		return
	}

	// Show cached update notification only if the cached version is strictly newer (semver).
	// A simple != check would incorrectly suggest a downgrade when the cached value is older
	// than the running binary (e.g. binary updated but cached state is stale).
	if state.AvailableUpdate != "" {
		newer, err := update.IsNewerVersion(Version, state.AvailableUpdate)
		if err == nil && newer {
			if silent {
				deferredUpdateNotification = state.AvailableUpdate
			} else {
				fmt.Printf("\n📦 Update available: %s → %s (run 'citadel update install')\n\n", Version, state.AvailableUpdate)
			}
		}
	}

	// Run background check if needed (non-blocking)
	if update.ShouldCheck(state) {
		go func() {
			client := update.NewClientWithTimeout(Version, 5*time.Second)
			release, err := client.CheckForUpdate()

			// Update state regardless of result
			update.UpdateLastCheck(state)
			if err == nil && release != nil {
				state.AvailableUpdate = release.TagName
			} else if err == nil {
				state.AvailableUpdate = "" // Clear if on latest
			}
			_ = update.SaveState(state)
		}()
	}
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	hideAdvancedCommands()
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

// GetRootCmd returns the root command for documentation generation.
func GetRootCmd() *cobra.Command {
	return rootCmd
}

func init() {
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.citadel-cli.yaml)")
	rootCmd.PersistentFlags().StringVar(&nexusURL, "nexus", "https://nexus.aceteam.ai", "The URL of the AceTeam Nexus server")
	rootCmd.PersistentFlags().StringVar(&authServiceURL, "auth-service", getEnvOrDefault("CITADEL_AUTH_HOST", "https://aceteam.ai"), "The URL of the authentication service")
	rootCmd.PersistentFlags().BoolVar(&debugMode, "debug", false, "Enable debug output")
	rootCmd.PersistentFlags().BoolVar(&noColorGlobal, "no-color", false, "Disable colorized output")
	rootCmd.PersistentFlags().BoolVar(&noAutoUpdate, "no-auto-update", false, "Disable automatic update checks and installs for this run (or set CITADEL_NO_AUTO_UPDATE=1)")
	rootCmd.Flags().BoolVar(&daemonMode, "daemon", false, "Run in background daemon mode (no TUI)")
	rootCmd.Flags().BoolVar(&noMouse, "no-mouse", false, "Disable control-center mouse control (keeps terminal drag-to-copy)")

	// Hide persistent flags that are only for dev/scripting
	rootCmd.PersistentFlags().MarkHidden("config")
	rootCmd.PersistentFlags().MarkHidden("nexus")
	rootCmd.PersistentFlags().MarkHidden("auth-service")
}

// hideAdvancedCommands marks dev/scripting commands as hidden from default help.
// Call this after all init() functions have registered their commands.
func hideAdvancedCommands() {
	visible := map[string]bool{
		"status":  true,
		"update":  true,
		"version": true,
		"help":    true,
		"mcp":     true,
	}
	for _, cmd := range rootCmd.Commands() {
		if !visible[cmd.Name()] {
			cmd.Hidden = true
		}
	}
}
