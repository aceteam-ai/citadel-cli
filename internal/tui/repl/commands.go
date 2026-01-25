// Package repl provides the interactive REPL with slash commands.
package repl

import (
	"fmt"
	"sort"
	"strings"
)

// Command represents a slash command
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Usage       string
	Handler     CommandHandler
	SubCommands []*Command
}

// CommandHandler is the function signature for command handlers
type CommandHandler func(args []string) error

// CommandRegistry holds all registered commands
type CommandRegistry struct {
	commands map[string]*Command
	aliases  map[string]string
}

// NewCommandRegistry creates a new command registry
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{
		commands: make(map[string]*Command),
		aliases:  make(map[string]string),
	}
}

// Register adds a command to the registry
func (r *CommandRegistry) Register(cmd *Command) {
	r.commands[cmd.Name] = cmd
	for _, alias := range cmd.Aliases {
		r.aliases[alias] = cmd.Name
	}
}

// Get retrieves a command by name or alias
func (r *CommandRegistry) Get(name string) *Command {
	if cmd, ok := r.commands[name]; ok {
		return cmd
	}
	if canonical, ok := r.aliases[name]; ok {
		return r.commands[canonical]
	}
	return nil
}

// List returns all registered commands
func (r *CommandRegistry) List() []*Command {
	var cmds []*Command
	for _, cmd := range r.commands {
		cmds = append(cmds, cmd)
	}
	sort.Slice(cmds, func(i, j int) bool {
		return cmds[i].Name < cmds[j].Name
	})
	return cmds
}

// Names returns all command names (not aliases, for cleaner display)
func (r *CommandRegistry) Names() []string {
	var names []string
	for name := range r.commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AllNames returns all command names and aliases
func (r *CommandRegistry) AllNames() []string {
	var names []string
	for name := range r.commands {
		names = append(names, name)
	}
	for alias := range r.aliases {
		names = append(names, alias)
	}
	sort.Strings(names)
	return names
}

// DefaultCommands returns the default set of commands for Citadel
func DefaultCommands() *CommandRegistry {
	registry := NewCommandRegistry()

	// /help - Show available commands
	registry.Register(&Command{
		Name:        "help",
		Aliases:     []string{"h", "?"},
		Description: "Show available commands",
		Usage:       "/help [command]",
		Handler: func(args []string) error {
			if len(args) > 0 {
				// Show help for specific command
				cmd := registry.Get(args[0])
				if cmd == nil {
					return fmt.Errorf("unknown command: %s", args[0])
				}
				fmt.Printf("\n%s - %s\n", cmd.Name, cmd.Description)
				if cmd.Usage != "" {
					fmt.Printf("Usage: %s\n", cmd.Usage)
				}
				if len(cmd.Aliases) > 0 {
					fmt.Printf("Aliases: %s\n", strings.Join(cmd.Aliases, ", "))
				}
				fmt.Println()
				return nil
			}

			// Show all commands
			fmt.Println("\nAvailable commands:")
			for _, cmd := range registry.List() {
				aliases := ""
				if len(cmd.Aliases) > 0 {
					aliases = " (" + strings.Join(cmd.Aliases, ", ") + ")"
				}
				fmt.Printf("  /%s%s - %s\n", cmd.Name, aliases, cmd.Description)
			}
			fmt.Println("\nType /help <command> for more information.")
			fmt.Println()
			return nil
		},
	})

	// /quit - Exit the REPL
	registry.Register(&Command{
		Name:        "quit",
		Aliases:     []string{"q", "exit"},
		Description: "Exit Citadel interactive mode",
		Usage:       "/quit",
		Handler: func(args []string) error {
			return ErrQuit
		},
	})

	// /status - Show node status
	registry.Register(&Command{
		Name:        "status",
		Aliases:     []string{"st", "info"},
		Description: "Show node status dashboard",
		Usage:       "/status",
		Handler:     nil, // Will be set by the REPL
	})

	// /services - List and manage services
	registry.Register(&Command{
		Name:        "services",
		Aliases:     []string{"svc"},
		Description: "List and manage services",
		Usage:       "/services [start|stop|restart <name>]",
		Handler:     nil, // Will be set by the REPL
	})

	// /logs - View service logs
	registry.Register(&Command{
		Name:        "logs",
		Aliases:     []string{"log"},
		Description: "View service logs",
		Usage:       "/logs <service> [--follow]",
		Handler:     nil, // Will be set by the REPL
	})

	// /peers - Show network peers
	registry.Register(&Command{
		Name:        "peers",
		Aliases:     []string{"nodes"},
		Description: "Show network peers",
		Usage:       "/peers",
		Handler:     nil, // Will be set by the REPL
	})

	// /jobs - Show job queue status
	registry.Register(&Command{
		Name:        "jobs",
		Aliases:     []string{"queue"},
		Description: "Show job queue status",
		Usage:       "/jobs",
		Handler:     nil, // Will be set by the REPL
	})

	// /clear - Clear the screen
	registry.Register(&Command{
		Name:        "clear",
		Aliases:     []string{"cls"},
		Description: "Clear the screen",
		Usage:       "/clear",
		Handler: func(args []string) error {
			// ANSI escape to clear screen and move cursor to top
			fmt.Print("\033[2J\033[H")
			return nil
		},
	})

	// /version - Show version
	registry.Register(&Command{
		Name:        "version",
		Aliases:     []string{"v"},
		Description: "Show Citadel version",
		Usage:       "/version",
		Handler:     nil, // Will be set by the REPL
	})

	return registry
}

// ErrQuit is returned when the user wants to quit the REPL
var ErrQuit = fmt.Errorf("quit")
