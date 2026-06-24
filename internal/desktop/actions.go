package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"time"
)

const actionTimeout = 5 * time.Second

type Action struct {
	Type   string `json:"type"`
	X      *int   `json:"x,omitempty"`
	Y      *int   `json:"y,omitempty"`
	Button *int   `json:"button,omitempty"`
	Text   string `json:"text,omitempty"`
	Key    string `json:"key,omitempty"`
	Delta  *int   `json:"delta,omitempty"`
	// Mode selects absolute (default, empty) or "relative" pointer movement for
	// the "move" action. Relative moves use Dx/Dy instead of X/Y. Issue #334.
	Mode string `json:"mode,omitempty"`
	// Dx/Dy are signed pixel offsets for a relative "move". Negative values move
	// left/up. Only used when Mode == "relative".
	Dx *int `json:"dx,omitempty"`
	Dy *int `json:"dy,omitempty"`
}

// relMoveBound caps the magnitude of a single relative move offset. Matches the
// absolute coordinate ceiling but allows negative offsets for left/up motion.
const relMoveBound = 32767

var allowedActionTypes = map[string]bool{
	"move":      true,
	"click":     true,
	"type":      true,
	"key":       true,
	"scroll":    true,
	"mousedown": true, // press a button without releasing (drag start; issue #4180)
	"mouseup":   true, // release a held button (drag end; issue #4180)
}

var safeKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_+\- ]+$`)
var safeTextPattern = regexp.MustCompile(`^[^\x00-\x08\x0e-\x1f\x7f]+$`)

// ParseActions parses and validates a JSON array of input actions.
func ParseActions(data []byte) ([]Action, error) {
	var actions []Action
	if err := json.Unmarshal(data, &actions); err != nil {
		return nil, fmt.Errorf("invalid action JSON: %w", err)
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("empty action array")
	}
	if len(actions) > 100 {
		return nil, fmt.Errorf("too many actions (max 100, got %d)", len(actions))
	}
	for i, a := range actions {
		if err := validateAction(a); err != nil {
			return nil, fmt.Errorf("action[%d]: %w", i, err)
		}
	}
	return actions, nil
}

func validateAction(a Action) error {
	if !allowedActionTypes[a.Type] {
		return fmt.Errorf("unknown action type %q (allowed: move, click, type, key, scroll, mousedown, mouseup)", a.Type)
	}

	switch a.Type {
	case "move":
		switch a.Mode {
		case "", "absolute":
			if a.X == nil || a.Y == nil {
				return fmt.Errorf("absolute move requires x and y coordinates")
			}
			if *a.X < 0 || *a.Y < 0 || *a.X > 32767 || *a.Y > 32767 {
				return fmt.Errorf("coordinates out of range (0-32767)")
			}
		case "relative":
			if a.Dx == nil || a.Dy == nil {
				return fmt.Errorf("relative move requires dx and dy offsets")
			}
			if *a.Dx < -relMoveBound || *a.Dx > relMoveBound || *a.Dy < -relMoveBound || *a.Dy > relMoveBound {
				return fmt.Errorf("relative offsets out of range (-%d to %d)", relMoveBound, relMoveBound)
			}
		default:
			return fmt.Errorf("unknown move mode %q (allowed: absolute, relative)", a.Mode)
		}
	case "click":
		if a.X == nil || a.Y == nil {
			return fmt.Errorf("click requires x and y coordinates")
		}
		if *a.X < 0 || *a.Y < 0 || *a.X > 32767 || *a.Y > 32767 {
			return fmt.Errorf("coordinates out of range (0-32767)")
		}
		if a.Button != nil && (*a.Button < 1 || *a.Button > 5) {
			return fmt.Errorf("button must be 1-5")
		}
	case "type":
		if a.Text == "" {
			return fmt.Errorf("type requires non-empty text")
		}
		if len(a.Text) > 1000 {
			return fmt.Errorf("text too long (max 1000 chars)")
		}
		if !safeTextPattern.MatchString(a.Text) {
			return fmt.Errorf("text contains invalid characters")
		}
	case "key":
		if a.Key == "" {
			return fmt.Errorf("key requires non-empty key name")
		}
		if len(a.Key) > 100 {
			return fmt.Errorf("key name too long")
		}
		if !safeKeyPattern.MatchString(a.Key) {
			return fmt.Errorf("key name contains invalid characters")
		}
	case "mousedown", "mouseup":
		// Press/release a button at the current pointer position. Coordinates
		// are not required: drag sequences emit a preceding "move" action to
		// position the pointer (see python translate_action left_click_drag).
		if a.Button != nil && (*a.Button < 1 || *a.Button > 5) {
			return fmt.Errorf("button must be 1-5")
		}
	case "scroll":
		if a.Delta == nil {
			return fmt.Errorf("scroll requires delta")
		}
		if *a.Delta == 0 {
			return fmt.Errorf("scroll delta must be non-zero")
		}
		if *a.Delta < -100 || *a.Delta > 100 {
			return fmt.Errorf("scroll delta out of range (-100 to 100)")
		}
	}

	return nil
}

// ActionToXdotoolArgs converts a validated action to xdotool arguments.
func ActionToXdotoolArgs(a Action) (string, []string, error) {
	switch a.Type {
	case "move":
		if a.Mode == "relative" {
			// mousemove_relative shifts the pointer by a signed offset from its
			// current position. The "--" guard lets negative dx/dy through as
			// operands rather than being parsed as flags. Issue #334.
			return "mousemove_relative", []string{"--", strconv.Itoa(*a.Dx), strconv.Itoa(*a.Dy)}, nil
		}
		return "mousemove", []string{"--", strconv.Itoa(*a.X), strconv.Itoa(*a.Y)}, nil
	case "click":
		button := buttonOrDefault(a.Button, 1)
		return "mousemove", []string{
			"--", strconv.Itoa(*a.X), strconv.Itoa(*a.Y),
			"click", strconv.Itoa(button),
		}, nil
	case "mousedown":
		// Press and hold a button at the current pointer position (drag start).
		return "mousedown", []string{strconv.Itoa(buttonOrDefault(a.Button, 1))}, nil
	case "mouseup":
		// Release a held button at the current pointer position (drag end).
		return "mouseup", []string{strconv.Itoa(buttonOrDefault(a.Button, 1))}, nil
	case "type":
		return "type", []string{"--clearmodifiers", "--", a.Text}, nil
	case "key":
		return "key", []string{"--clearmodifiers", a.Key}, nil
	case "scroll":
		button := 5
		clicks := -*a.Delta
		if *a.Delta > 0 {
			button = 4
			clicks = *a.Delta
		}
		args := make([]string, 0, clicks)
		for i := 0; i < clicks; i++ {
			args = append(args, strconv.Itoa(button))
		}
		return "click", args, nil
	default:
		return "", nil, fmt.Errorf("unsupported action type: %s", a.Type)
	}
}

func buttonOrDefault(b *int, def int) int {
	if b != nil {
		return *b
	}
	return def
}

// ExecuteActions executes a slice of validated actions on the current display.
func ExecuteActions(ctx context.Context, actions []Action) error {
	switch runtime.GOOS {
	case "linux":
		return executeLinuxActions(ctx, actions)
	case "darwin":
		// TODO: cliclick for macOS
		return fmt.Errorf("actions not implemented on macOS")
	case "windows":
		// TODO: PowerShell/SendInput for Windows
		return fmt.Errorf("actions not implemented on Windows")
	default:
		return fmt.Errorf("actions not supported on %s", runtime.GOOS)
	}
}

func executeLinuxActions(ctx context.Context, actions []Action) error {
	display := os.Getenv("DISPLAY")
	if display == "" {
		display = ":0"
	}

	env := append(os.Environ(), "DISPLAY="+display)

	xdotoolPath, err := exec.LookPath("xdotool")
	if err != nil {
		return fmt.Errorf("xdotool not found (install with: apt-get install xdotool)")
	}

	for i, action := range actions {
		subcmd, args, err := ActionToXdotoolArgs(action)
		if err != nil {
			return fmt.Errorf("action[%d]: %w", i, err)
		}

		cmdArgs := append([]string{subcmd}, args...)
		actionCtx, cancel := context.WithTimeout(ctx, actionTimeout)
		cmd := exec.CommandContext(actionCtx, xdotoolPath, cmdArgs...)
		cmd.Env = env

		if output, err := cmd.CombinedOutput(); err != nil {
			cancel()
			return fmt.Errorf("action[%d] (%s) failed: %w (output: %s)", i, action.Type, err, string(output))
		}
		cancel()
	}

	return nil
}
