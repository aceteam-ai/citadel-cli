package repl

import (
	"sort"
	"strings"
)

// Completer provides tab completion for the REPL
type Completer struct {
	registry *CommandRegistry
	services []string
}

// NewCompleter creates a new completer
func NewCompleter(registry *CommandRegistry) *Completer {
	return &Completer{
		registry: registry,
	}
}

// SetServices updates the list of known services for completion
func (c *Completer) SetServices(services []string) {
	c.services = services
}

// Complete returns completion suggestions for the given input
func (c *Completer) Complete(input string) []string {
	input = strings.TrimSpace(input)

	// Empty input - suggest starting with /
	if input == "" {
		return []string{"/"}
	}

	// Not a command - no suggestions
	if !strings.HasPrefix(input, "/") {
		return nil
	}

	// Remove the leading /
	input = strings.TrimPrefix(input, "/")
	parts := strings.Fields(input)

	// Completing the command name
	if len(parts) == 0 || (len(parts) == 1 && !strings.HasSuffix(input, " ")) {
		prefix := ""
		if len(parts) == 1 {
			prefix = parts[0]
		}
		return c.completeCommand(prefix)
	}

	// Completing arguments for a command
	cmdName := parts[0]
	cmd := c.registry.Get(cmdName)
	if cmd == nil {
		return nil
	}

	// Get the last partial argument
	argPrefix := ""
	if !strings.HasSuffix(input, " ") && len(parts) > 1 {
		argPrefix = parts[len(parts)-1]
	}

	return c.completeArgs(cmd, parts[1:], argPrefix)
}

// completeCommand returns command name completions
func (c *Completer) completeCommand(prefix string) []string {
	var matches []string

	for _, name := range c.registry.Names() {
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, "/"+name)
		}
	}

	sort.Strings(matches)
	return matches
}

// completeArgs returns argument completions for a command
func (c *Completer) completeArgs(cmd *Command, args []string, prefix string) []string {
	switch cmd.Name {
	case "logs", "services":
		// Complete service names
		return c.completeServices(prefix)
	case "help":
		// Complete command names
		return c.completeCommand(prefix)
	}

	return nil
}

// completeServices returns service name completions
func (c *Completer) completeServices(prefix string) []string {
	var matches []string

	for _, svc := range c.services {
		if strings.HasPrefix(svc, prefix) {
			matches = append(matches, svc)
		}
	}

	// Also add common service names if no services are known
	if len(c.services) == 0 {
		defaultServices := []string{"vllm", "ollama", "llamacpp", "lmstudio"}
		for _, svc := range defaultServices {
			if strings.HasPrefix(svc, prefix) {
				matches = append(matches, svc)
			}
		}
	}

	sort.Strings(matches)
	return matches
}

// FindLongestCommonPrefix finds the longest common prefix among suggestions
func FindLongestCommonPrefix(suggestions []string) string {
	if len(suggestions) == 0 {
		return ""
	}
	if len(suggestions) == 1 {
		return suggestions[0]
	}

	prefix := suggestions[0]
	for _, s := range suggestions[1:] {
		for i := 0; i < len(prefix) && i < len(s); i++ {
			if prefix[i] != s[i] {
				prefix = prefix[:i]
				break
			}
		}
		if len(s) < len(prefix) {
			prefix = prefix[:len(s)]
		}
	}

	return prefix
}
