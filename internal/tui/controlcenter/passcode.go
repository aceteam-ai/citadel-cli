// Package controlcenter: node-passcode set/rotate/clear controls (citadel#760).
//
// Before this file, the Control Center could only WARN when a sensitive
// remote-access surface (Console/Desktop/Files) was enabled with no node
// passcode set (see the "enabled but no node passcode is set" line in
// actions.go's showBuiltinServicesModal) and pointed the operator elsewhere
// (the web console, APPLY_DEVICE_CONFIG, or 'citadel passcode set' at a
// shell). This file gives the TUI itself the ability to fix it, reusing the
// same config.Permissions helpers citadel#753/#755 shipped for the CLI
// ('citadel passcode set'/'clear', cmd/passcode.go): SetPasscode + bcrypt
// hashing, HasPasscode, and SavePermissions. No worker restart is needed:
// every gate (terminal server, gateway, SHELL_COMMAND handler) reloads
// permissions.yaml per connection/request.
package controlcenter

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// validatePasscodeInput checks a set/rotate passcode entry pair before it is
// persisted. Mirrors 'citadel passcode set's confirmation rule
// (cmd/passcode.go's matchPasscodeConfirmation + empty-pin guard): neither
// entry may be empty/whitespace-only, and the two entries must match exactly,
// so an interactive typo does not silently lock the node behind a passcode
// the operator never actually typed. Pure and side-effect free so it can be
// unit-tested without a running TUI.
func validatePasscodeInput(pin, confirm string) error {
	pin = strings.TrimSpace(pin)
	confirm = strings.TrimSpace(confirm)
	if pin == "" {
		return fmt.Errorf("passcode must not be empty")
	}
	if pin != confirm {
		return fmt.Errorf("passcodes did not match")
	}
	return nil
}

// setNodePasscode hashes and persists pin as the node's passcode via the
// PermissionsCallbacks Load/Save pair (the same permissions.yaml the CLI's
// 'citadel passcode set' and an APPLY_DEVICE_CONFIG push write). Returns the
// updated permissions so callers can report follow-on state (e.g. whether any
// sensitive surface is actually enabled) without a second load. Never logs or
// otherwise persists pin itself; only the bcrypt hash is written to disk.
func (cc *ControlCenter) setNodePasscode(pin string) (*config.Permissions, error) {
	if cc.permissions.Load == nil || cc.permissions.Save == nil {
		return nil, fmt.Errorf("permissions not configured")
	}
	perms := cc.permissions.Load()
	if err := perms.SetPasscode(pin); err != nil {
		return nil, fmt.Errorf("set passcode: %w", err)
	}
	if err := cc.permissions.Save(perms); err != nil {
		return nil, fmt.Errorf("save permissions: %w", err)
	}
	return perms, nil
}

// clearNodePasscode removes the node passcode via the PermissionsCallbacks
// Load/Save pair. Clearing re-locks every enabled sensitive surface
// (Console/Desktop/Files): config.Permissions.VerifyPasscode fails CLOSED with
// no hash stored, so an enabled-but-ungated surface is never a possible state.
func (cc *ControlCenter) clearNodePasscode() (*config.Permissions, error) {
	if cc.permissions.Load == nil || cc.permissions.Save == nil {
		return nil, fmt.Errorf("permissions not configured")
	}
	perms := cc.permissions.Load()
	_ = perms.SetPasscode("") // empty pin only clears; SetPasscode never errors on this path
	if err := cc.permissions.Save(perms); err != nil {
		return nil, fmt.Errorf("save permissions: %w", err)
	}
	return perms, nil
}

// showPasscodeModal shows the current node-passcode status and offers to set,
// rotate, or clear it. Reachable from the Built-in Services modal (the same
// place that shows the "enabled but no node passcode is set" warning) via the
// P hotkey, so the surface that flags the problem can also fix it.
func (cc *ControlCenter) showPasscodeModal() {
	if cc.permissions.Load == nil || cc.permissions.Save == nil {
		cc.AddActivity("warning", "Permissions not configured")
		return
	}

	cc.inModal = true
	perms := cc.permissions.Load()

	var statusLine, body string
	buttons := []string{"Cancel"}
	if perms.HasPasscode() {
		statusLine = "[green::b]Set[-:-:-]"
		body = "Rotating replaces it immediately; every sensitive surface keeps\n" +
			"working, gated by the new passcode. Clearing removes it and\n" +
			"re-locks every enabled sensitive surface until a new one is set."
		buttons = append(buttons, "Rotate", "Clear")
	} else {
		statusLine = "[red::b]Not set[-:-:-]"
		body = "No node passcode is set. Console, Desktop, and Files fail CLOSED\n" +
			"(access denied) even when enabled, until you set one here."
		buttons = append(buttons, "Set")
	}

	modal := tview.NewModal().
		SetText(fmt.Sprintf("[yellow::b]Node Passcode[-:-:-]\n\nStatus: %s\n\n%s", statusLine, body)).
		AddButtons(buttons).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cc.inModal = false
			switch buttonLabel {
			case "Set", "Rotate":
				cc.app.SetRoot(cc.rootView, true)
				cc.showPasscodeInputModal(buttonLabel == "Rotate")
			case "Clear":
				cc.app.SetRoot(cc.rootView, true)
				cc.showPasscodeClearConfirmModal()
			default:
				cc.app.SetRoot(cc.rootView, true)
				cc.updatePaneFocus()
			}
		})

	cc.app.SetRoot(modal, true)
	cc.app.SetFocus(modal)
}

// showPasscodeInputModal prompts for a new node passcode with echo disabled
// (tview's InputField mask character, matching 'citadel passcode set's
// term.ReadPassword no-echo prompt) and a required matching confirmation
// entry, then persists it. The typed PIN lives only in the two InputFields'
// in-memory buffers and this function's local variables. It is cleared from
// both fields immediately after use, and it is never written to the activity
// feed, never logged, and never passed as a command-line argument (this is a
// pure in-process config write via setNodePasscode, not a subprocess call).
func (cc *ControlCenter) showPasscodeInputModal(rotate bool) {
	cc.inModal = true

	title := "Set Node Passcode"
	if rotate {
		title = "Rotate Node Passcode"
	}

	pinInput := tview.NewInputField().
		SetLabel("New Passcode: ").
		SetFieldWidth(20).
		SetMaskCharacter('*')
	confirmInput := tview.NewInputField().
		SetLabel("Confirm:      ").
		SetFieldWidth(20).
		SetMaskCharacter('*')

	errView := tview.NewTextView().SetDynamicColors(true)

	formFlex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 1, 0, false).
		AddItem(pinInput, 1, 0, true).
		AddItem(nil, 1, 0, false).
		AddItem(confirmInput, 1, 0, false).
		AddItem(nil, 1, 0, false).
		AddItem(errView, 1, 0, false).
		AddItem(nil, 1, 0, false).
		AddItem(tview.NewTextView().SetText("[yellow]Tab[-] switch field  [yellow]Enter[-] save  [yellow]Esc[-] cancel").SetDynamicColors(true).SetTextAlign(tview.AlignCenter), 1, 0, false)

	formFlex.SetBorder(true).SetTitle(" " + title + " ")

	centered := tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(formFlex, 12, 0, true).
			AddItem(nil, 0, 1, false), 50, 0, true).
		AddItem(nil, 0, 1, false)

	currentField := 0 // 0 = pin, 1 = confirm

	// wipeFields clears both InputFields' text so the plaintext PIN does not
	// linger in the widget buffers after we're done with them (cancel or
	// successful save).
	wipeFields := func() {
		pinInput.SetText("")
		confirmInput.SetText("")
	}

	closeModal := func() {
		wipeFields()
		cc.inModal = false
		cc.app.SetRoot(cc.rootView, true)
		cc.updatePaneFocus()
	}

	submit := func() {
		pin := pinInput.GetText()
		confirm := confirmInput.GetText()
		if err := validatePasscodeInput(pin, confirm); err != nil {
			wipeFields()
			cc.app.SetFocus(pinInput)
			currentField = 0
			errView.SetText(fmt.Sprintf("[red]%s[-]", err))
			return
		}

		perms, err := cc.setNodePasscode(pin)
		wipeFields()
		if err != nil {
			errView.SetText(fmt.Sprintf("[red]Failed to save: %v[-]", err))
			return
		}

		closeModal()
		verb := "set"
		if rotate {
			verb = "updated"
		}
		cc.AddActivity("success", fmt.Sprintf("Node passcode %s.", verb))
		if !(perms.Console || perms.Desktop || perms.Files) {
			cc.AddActivity("info", "Console, Desktop, and Files are all currently disabled, so this passcode has nothing to gate yet.")
		}
	}

	handleInput := func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			closeModal()
			return nil
		case tcell.KeyEnter:
			submit()
			return nil
		case tcell.KeyTab, tcell.KeyDown:
			if currentField == 0 {
				currentField = 1
				cc.app.SetFocus(confirmInput)
			} else {
				currentField = 0
				cc.app.SetFocus(pinInput)
			}
			return nil
		case tcell.KeyBacktab, tcell.KeyUp:
			if currentField == 1 {
				currentField = 0
				cc.app.SetFocus(pinInput)
			} else {
				currentField = 1
				cc.app.SetFocus(confirmInput)
			}
			return nil
		}
		return event
	}

	pinInput.SetInputCapture(handleInput)
	confirmInput.SetInputCapture(handleInput)

	cc.app.SetRoot(centered, true)
	cc.app.SetFocus(pinInput)
}

// showPasscodeClearConfirmModal confirms before clearing the node passcode,
// which re-locks every enabled sensitive surface (Console/Desktop/Files).
func (cc *ControlCenter) showPasscodeClearConfirmModal() {
	cc.inModal = true

	modal := tview.NewModal().
		SetText("[red::b]Clear node passcode?[-:-:-]\n\n" +
			"Console, Desktop, and Files (if enabled) will fail closed\n" +
			"immediately (no worker restart needed) until a new\n" +
			"passcode is set.\n\nAre you sure?").
		AddButtons([]string{"Cancel", "Clear"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cc.inModal = false
			cc.app.SetRoot(cc.rootView, true)
			if buttonLabel == "Clear" {
				cc.doClearNodePasscode()
			} else {
				cc.updatePaneFocus()
			}
		})

	cc.app.SetRoot(modal, true)
	cc.app.SetFocus(modal)
}

// passcodeClearNeedsWarning reports whether clearing the passcode leaves any
// sensitive remote-access surface (Console/Desktop/Files) enabled but now
// unreachable, so the "enabled but no node passcode is set" warning
// (actions.go's showBuiltinServicesModal toggle handler) applies again. Pure
// so the "refresh the warning state" behavior is unit-testable without a
// running TUI.
//
// Deliberately matches the existing toggle-warning's surface set (Console,
// Desktop, Files) and 'citadel passcode clear's own message
// (cmd/passcode.go's runPasscodeClear), not config.Permissions.Shell, even
// though Shell is also passcode-gated in enforcement
// (internal/jobs/shell_command.go calls VerifyPasscode). Extending the
// warning to Shell is a real gap, but it is a pre-existing one shared by both
// of those call sites, out of scope for citadel#760's ask (give the TUI a way
// to set/rotate/clear), and tracked separately (citadel#763) rather than
// fixed as a side effect here.
func passcodeClearNeedsWarning(perms *config.Permissions) bool {
	return perms.Console || perms.Desktop || perms.Files
}

// builtinServicesActionDesc renders the Actions-panel description for the
// Built-in Services row (key 1). It appends a passcode warning when a
// sensitive surface is enabled but no passcode is set, so an operator who
// never opens the modal still sees the gap on the always-visible action
// panel, not only inside showBuiltinServicesModal's detail pane.
func builtinServicesActionDesc(perms *config.Permissions) string {
	enabled := 0
	for _, e := range []bool{perms.Console, perms.Desktop, perms.Files, perms.Services, perms.SSH} {
		if e {
			enabled++
		}
	}
	desc := fmt.Sprintf("[gray]%d/5 enabled[-]", enabled)
	if passcodeClearNeedsWarning(perms) && !perms.HasPasscode() {
		desc += "  [red]no passcode[-]"
	}
	return desc
}

// doClearNodePasscode performs the passcode clear and reports the resulting
// state, including re-surfacing the "enabled but no node passcode is set"
// warning (matching 'citadel passcode clear's copy) when a sensitive surface
// is still enabled, since clearing just re-locked it.
func (cc *ControlCenter) doClearNodePasscode() {
	perms, err := cc.clearNodePasscode()
	if err != nil {
		cc.AddActivity("error", fmt.Sprintf("Failed to clear passcode: %v", err))
		return
	}
	cc.AddActivity("success", "Node passcode cleared.")
	if passcodeClearNeedsWarning(perms) {
		cc.AddActivity("warning", "Console, Desktop, and/or Files are enabled; they now fail closed (access denied) until a new passcode is set.")
	}
	cc.updatePaneFocus()
}
