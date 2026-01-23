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

// logFile is the file handle for logging
var logFile *os.File
var logMu sync.Mutex
var logInitOnce sync.Once
var logDir string

// initLogFile initializes the log file with timestamp naming and symlink
func initLogFile() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}

	logDir = filepath.Join(homeDir, ".citadel-cli", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return
	}

	// Create timestamped log file: citadel-2026-01-22T11-04-47.log
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	logFileName := fmt.Sprintf("citadel-%s.log", timestamp)
	logPath := filepath.Join(logDir, logFileName)

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}

	logFile = f

	// Create/update symlink to latest log
	latestLink := filepath.Join(logDir, "latest.log")
	// Remove existing symlink if present
	os.Remove(latestLink)
	// Create new symlink (use relative path for portability)
	os.Symlink(logFileName, latestLink)

	// Write session header
	sessionTime := time.Now().Format("15:04:05")
	fmt.Fprintf(logFile, "[%s] [CITADEL] === Session started ===\n", sessionTime)
}

// Log writes a message to the log file and optionally to console if debug mode is enabled.
// This function always writes to the log file, but only prints to console with --debug.
func Log(format string, args ...interface{}) {
	timeStr := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)

	// Always write to file
	logMu.Lock()
	logInitOnce.Do(initLogFile)
	if logFile != nil {
		fmt.Fprintf(logFile, "[%s] [CITADEL] %s\n", timeStr, msg)
	}
	logMu.Unlock()

	// Print to console only if debug mode is enabled
	if debugMode {
		fmt.Printf("[%s] [CITADEL] %s\n", timeStr, msg)
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
	Long: `A self-contained agent and CLI for connecting your hardware
to the AceTeam control plane, making your resources available to your private workflows.`,
	Version: Version,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
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
