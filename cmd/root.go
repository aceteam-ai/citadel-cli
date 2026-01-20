// cmd/root.go
/*
Copyright © 2025 AceTeam <dev@aceteam.ai>
*/
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

// debugLogFile is the file handle for debug logging
var debugLogFile *os.File
var debugLogMu sync.Mutex
var debugLogInitOnce sync.Once

// initDebugLogFile initializes the debug log file
func initDebugLogFile() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}

	logDir := filepath.Join(homeDir, ".citadel-cli", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return
	}

	logPath := filepath.Join(logDir, "debug.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}

	debugLogFile = f

	// Write session header
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	fmt.Fprintf(debugLogFile, "\n=== Debug session started: %s ===\n", timestamp)
}

// Debug prints a message if debug mode is enabled and writes to log file
func Debug(format string, args ...interface{}) {
	if debugMode {
		timestamp := time.Now().Format("2006-01-02 15:04:05.000")
		msg := fmt.Sprintf(format, args...)

		// Print to console
		fmt.Printf("[DEBUG] %s\n", msg)

		// Write to file with timestamp
		debugLogMu.Lock()
		debugLogInitOnce.Do(initDebugLogFile)
		if debugLogFile != nil {
			fmt.Fprintf(debugLogFile, "[%s] %s\n", timestamp, msg)
		}
		debugLogMu.Unlock()
	}
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "citadel",
	Short: "Citadel is the agent for the AceTeam Sovereign Compute Fabric",
	Long: `A self-contained agent and CLI for connecting your hardware
to the AceTeam control plane, making your resources available to your private workflows.`,
	Version: Version,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		if debugMode {
			// Log the full command that was run
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
			Debug("command: %s", fullCmd)
		}
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.citadel-cli.yaml)")
	rootCmd.PersistentFlags().StringVar(&nexusURL, "nexus", "https://nexus.aceteam.ai", "The URL of the AceTeam Nexus server")
	rootCmd.PersistentFlags().StringVar(&authServiceURL, "auth-service", getEnvOrDefault("CITADEL_AUTH_HOST", "https://aceteam.ai"), "The URL of the authentication service")
	rootCmd.PersistentFlags().BoolVar(&debugMode, "debug", false, "Enable debug output")

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	// rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
