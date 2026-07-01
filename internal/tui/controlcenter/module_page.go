package controlcenter

import (
	"fmt"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ConfigField describes a single user-configurable env var for a module, mapped
// from catalog.ConfigVar in the cmd layer so this page never imports catalog
// internals.
type ConfigField struct {
	Name        string
	Description string
	Default     string
	Required    bool
}

// ModuleRisk is a single compose risk finding surfaced to the TUI (mapped from
// catalog.ComposeRisk in the cmd layer so this page imports no catalog internals).
type ModuleRisk struct {
	Critical  bool // true = Critical severity, false = High
	Directive string
	Detail    string
}

// ModuleResolveResult is what Resolve returns: the resolved module name, the
// container image (display-only), the config fields the form should collect, the
// trust state of the source, and the compose risk findings.
type ModuleResolveResult struct {
	Name            string
	Image           string
	Config          []ConfigField
	Trusted         bool
	Risks           []ModuleRisk
	HasCriticalRisk bool
}

// ModuleSource is one selectable row in the known/approved sources list. It maps
// a curated catalog/index entry (or a special in-app module like WhatsApp) into a
// display row so the user picks from a seeded list instead of typing a source
// blind. Mapped from catalog entries in the cmd layer so this page imports no
// catalog internals.
type ModuleSource struct {
	// Name is the module's display/catalog name (e.g. "vllm").
	Name string
	// Source is the value fed to Resolve when the row is chosen (catalog name,
	// owner/repo, or git URL). Empty for special rows.
	Source string
	// Description is a short human summary shown next to the name.
	Description string
	// Trusted marks a first-party/curated source (rendered with a check).
	Trusted bool
	// Special, when non-empty, names an in-app module page to switch to instead of
	// running the install form (e.g. "whatsapp" folds the WhatsApp bridge tab in).
	Special string
}

// ModuleInstallCallbacks holds the hooks for the "Install module" page. They are
// wired from cmd so the page stays free of catalog/network/docker imports.
//
// ListSources returns the seeded known/approved sources to pre-populate the
// picker (curated catalog/index entries + any special in-app modules). It is
// fail-soft: an empty slice just yields a blank list, never an error.
//
// Resolve clones/updates the source repo (or loads a catalog name) and returns
// the module name + required config + trust/risk info; it may block on the
// network, so the page calls it off the UI goroutine. Install performs the actual
// install with the collected config passed as overrides (the non-interactive
// installer path, so no stdin is read). allowPrivileged must be true for the
// install to proceed when the compose has a Critical risk; the page only sets it
// when the user explicitly opts in.
type ModuleInstallCallbacks struct {
	ListSources func() []ModuleSource
	Resolve     func(source string) (ModuleResolveResult, error)
	Install     func(source string, overrides map[string]string, allowPrivileged bool) (installedName string, err error)
	// SelectSpecial switches the control center to the named in-app module page
	// (e.g. "whatsapp"). Wired from Run() so the page needs no PageManager ref.
	SelectSpecial func(name string)
}

// allowPrivilegedLabel is the form checkbox shown when a Critical risk is found.
const allowPrivilegedLabel = "Allow privileged (root-equivalent) access"

// modulePhase tracks the two-phase form flow.
type modulePhase int

const (
	phaseSource modulePhase = iota // collecting the source string
	phaseConfig                    // source resolved, collecting config
	phaseDone                      // install finished (success or error)
)

// ModulePage implements the Page interface for installing a service module from
// any standardized git repo. It is a one-shot form (no polling): the user types a
// source, presses Resolve, fills any required config, then Install.
type ModulePage struct {
	app *tview.Application
	cb  ModuleInstallCallbacks

	// UI
	root       *tview.Flex
	sourceList *tview.List
	form       *tview.Form
	status     *tview.TextView

	// State
	mu       sync.Mutex
	phase    modulePhase
	source   string
	resolved ModuleResolveResult
	busy     bool
	busyMsg  string
	message  string // last success/info message
	errMsg   string // last error message
	sources  []ModuleSource
}

// NewModulePage creates an Install-module page wired to the given callbacks.
func NewModulePage(cb ModuleInstallCallbacks) *ModulePage {
	return &ModulePage{cb: cb}
}

// Name implements Page.
func (p *ModulePage) Name() string { return "module" }

// Title implements Page.
func (p *ModulePage) Title() string { return "Install Module" }

// Build implements Page.
func (p *ModulePage) Build(app *tview.Application) tview.Primitive {
	p.app = app

	// Known/approved sources list (left column). Pre-populated on activate so the
	// user picks a module instead of typing a source into a blank input blind.
	p.sourceList = tview.NewList().ShowSecondaryText(true)
	p.sourceList.SetBorder(true)
	p.sourceList.SetTitle(" Known modules ")
	p.sourceList.SetTitleAlign(tview.AlignLeft)

	p.form = tview.NewForm()
	p.form.SetBorder(true)
	p.form.SetTitle(" Install module ")
	p.form.SetTitleAlign(tview.AlignLeft)

	p.status = tview.NewTextView()
	p.status.SetDynamicColors(true)
	p.status.SetScrollable(true)
	p.status.SetBorder(true)
	p.status.SetTitle(" Status ")
	p.status.SetTitleAlign(tview.AlignLeft)

	p.root = tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(p.sourceList, 0, 1, true).
		AddItem(p.form, 0, 2, false).
		AddItem(p.status, 0, 2, false)

	p.buildSourceForm()
	p.render()
	return p.root
}

// OnActivate implements Page. Seeds the sources list (fail-soft) and focuses it
// so the user lands on the pre-populated picker, not a blank input.
func (p *ModulePage) OnActivate() {
	p.loadSources()
	if p.app != nil && p.sourceList != nil {
		p.app.SetFocus(p.sourceList)
	}
}

// OnDeactivate implements Page.
func (p *ModulePage) OnDeactivate() {}

// loadSources fills the list from the ListSources hook. Each normal row, when
// chosen, drops its source into the form input and focuses the form so the user
// can Resolve/Install. A special row (e.g. WhatsApp) switches to that in-app
// module page instead.
func (p *ModulePage) loadSources() {
	if p.sourceList == nil {
		return
	}
	var srcs []ModuleSource
	if p.cb.ListSources != nil {
		srcs = p.cb.ListSources()
	}
	p.mu.Lock()
	p.sources = srcs
	p.mu.Unlock()

	p.sourceList.Clear()
	for _, s := range srcs {
		primary := s.Name
		if s.Trusted {
			primary = "✓ " + primary
		}
		secondary := s.Description
		sel := s // capture
		p.sourceList.AddItem(primary, secondary, 0, func() { p.chooseSource(sel) })
	}
	if len(srcs) == 0 {
		p.sourceList.AddItem("(no seeded modules)", "type a source in the form on the right", 0, nil)
	}
}

// chooseSource applies a picked list row: a special row switches pages; a normal
// row seeds the source input and moves focus to the form.
func (p *ModulePage) chooseSource(s ModuleSource) {
	if s.Special != "" {
		if p.cb.SelectSpecial != nil {
			p.cb.SelectSpecial(s.Special)
		}
		return
	}
	p.mu.Lock()
	p.source = s.Source
	p.mu.Unlock()
	// Rebuild the source form so the seeded value shows in the input.
	p.buildSourceForm()
	if p.app != nil && p.form != nil {
		p.app.SetFocus(p.form)
	}
	p.render()
}

// nextFormFocus computes the next focus index within a tview.Form's LINEAR focus
// space (form items first, then buttons — matching Form.SetFocus). cur is the
// current linear index, total is itemCount+buttonCount, and forward selects Tab
// (true) vs Shift+Tab (false). It returns (nextIndex, atBoundary): atBoundary is
// true when moving off the first (backward) or last (forward) element, which is
// the signal to hand Tab off to the PageManager for tab-switching.
//
// Pure function so the actual reported bug — Tab wrongly leaving the form mid-way
// vs correctly cycling fields — is unit-testable without a running app.
func nextFormFocus(cur, total int, forward bool) (int, bool) {
	if total <= 0 {
		return cur, true
	}
	if forward {
		if cur >= total-1 {
			return cur, true
		}
		return cur + 1, false
	}
	if cur <= 0 {
		return cur, true
	}
	return cur - 1, false
}

// currentFormFocus returns the current linear focus index of the form (items
// first, then buttons), or 0 when nothing is focused yet.
func (p *ModulePage) currentFormFocus() int {
	if p.form == nil {
		return 0
	}
	idx, btn := p.form.GetFocusedItemIndex()
	if btn >= 0 {
		return p.form.GetFormItemCount() + btn
	}
	if idx < 0 {
		return 0
	}
	return idx
}

// formTotal returns the count of focusable form elements (fields + buttons).
func (p *ModulePage) formTotal() int {
	if p.form == nil {
		return 0
	}
	return p.form.GetFormItemCount() + p.form.GetButtonCount()
}

// HandleInput implements Page. It OWNS intra-page Tab navigation (it must — a
// SetInputCapture handler that returns the event to let the form cycle fields
// natively would be intercepted by the PageManager's bubble check first, so the
// page advances form focus itself and returns the event ONLY at the true
// first/last boundary). Tab/Shift+Tab cycle sources list ↔ form fields ↔ tabs;
// Escape defocuses the form back to the list so an input never traps the user.
func (p *ModulePage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	if p.app == nil {
		return event
	}
	onList := p.app.GetFocus() == p.sourceList

	switch event.Key() {
	case tcell.KeyTab:
		if onList {
			// List → first form element.
			p.app.SetFocus(p.form)
			return nil
		}
		next, boundary := nextFormFocus(p.currentFormFocus(), p.formTotal(), true)
		if boundary {
			return event // hand off: PageManager switches to the next tab
		}
		p.form.SetFocus(next)
		p.app.SetFocus(p.form)
		return nil
	case tcell.KeyBacktab:
		if onList {
			return event // hand off: PageManager switches to the previous tab
		}
		next, boundary := nextFormFocus(p.currentFormFocus(), p.formTotal(), false)
		if boundary {
			// Off the first form element → back to the sources list.
			p.app.SetFocus(p.sourceList)
			return nil
		}
		p.form.SetFocus(next)
		p.app.SetFocus(p.form)
		return nil
	case tcell.KeyEsc:
		if !onList {
			p.app.SetFocus(p.sourceList)
			return nil
		}
		return event
	}
	return event
}

// buildSourceForm builds the phase-1 form: a single source input + Resolve button.
func (p *ModulePage) buildSourceForm() {
	p.form.Clear(true)
	p.form.AddInputField("Source", p.source, 40, nil, func(text string) {
		p.mu.Lock()
		p.source = text
		p.mu.Unlock()
	})
	p.form.AddButton("Resolve", func() { p.doResolve() })
	p.form.AddButton("Reset", func() { p.reset() })
}

// buildConfigForm builds the phase-2 form: one input per config field + Install.
func (p *ModulePage) buildConfigForm() {
	p.form.Clear(true)
	for _, f := range p.resolved.Config {
		label := f.Name
		if f.Required && f.Default == "" {
			label += " *"
		}
		p.form.AddInputField(label, f.Default, 40, nil, nil)
	}
	// When the compose has a Critical risk, the user must explicitly opt in to a
	// privileged install; this checkbox is the TUI equivalent of --allow-privileged.
	if p.resolved.HasCriticalRisk {
		p.form.AddCheckbox(allowPrivilegedLabel, false, nil)
	}
	p.form.AddButton("Install", func() { p.doInstall() })
	p.form.AddButton("Back", func() {
		p.mu.Lock()
		p.phase = phaseSource
		p.errMsg = ""
		p.mu.Unlock()
		p.buildSourceForm()
		p.render()
	})
}

// collectOverrides reads the current config-form inputs into an overrides map.
func (p *ModulePage) collectOverrides() map[string]string {
	overrides := make(map[string]string)
	for _, f := range p.resolved.Config {
		label := f.Name
		if f.Required && f.Default == "" {
			label += " *"
		}
		item := p.form.GetFormItemByLabel(label)
		input, ok := item.(*tview.InputField)
		if !ok {
			continue
		}
		val := strings.TrimSpace(input.GetText())
		if val != "" {
			overrides[f.Name] = val
		}
	}
	return overrides
}

// doResolve resolves the source off the UI goroutine, then rebuilds the form
// with the module's config fields.
func (p *ModulePage) doResolve() {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return
	}
	src := strings.TrimSpace(p.source)
	if src == "" {
		p.errMsg = "Enter a module source (catalog name, owner/repo, or git URL)."
		p.mu.Unlock()
		p.render()
		return
	}
	p.busy = true
	p.busyMsg = "Resolving source (cloning/updating the module repo)..."
	p.errMsg = ""
	p.message = ""
	p.mu.Unlock()
	p.render()

	go func() {
		var res ModuleResolveResult
		var err error
		if p.cb.Resolve != nil {
			res, err = p.cb.Resolve(src)
		} else {
			err = fmt.Errorf("module install is not available")
		}

		p.mu.Lock()
		p.busy = false
		p.busyMsg = ""
		if err != nil {
			p.errMsg = err.Error()
			p.mu.Unlock()
			p.queueRender()
			return
		}
		p.resolved = res
		p.phase = phaseConfig
		p.mu.Unlock()

		if p.app != nil {
			p.app.QueueUpdateDraw(func() {
				p.buildConfigForm()
				p.render()
			})
		}
	}()
}

// doInstall collects config and runs the install off the UI goroutine.
func (p *ModulePage) doInstall() {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return
	}
	src := p.source
	overrides := p.collectOverrides()

	// Validate that every required field with no default has a value.
	var missing []string
	for _, f := range p.resolved.Config {
		if f.Required && f.Default == "" {
			if _, ok := overrides[f.Name]; !ok {
				missing = append(missing, f.Name)
			}
		}
	}
	if len(missing) > 0 {
		p.errMsg = "Missing required config: " + strings.Join(missing, ", ")
		p.mu.Unlock()
		p.render()
		return
	}

	// Read the privileged opt-in checkbox (only present when HasCriticalRisk).
	allowPrivileged := false
	if p.resolved.HasCriticalRisk {
		if cb, ok := p.form.GetFormItemByLabel(allowPrivilegedLabel).(*tview.Checkbox); ok {
			allowPrivileged = cb.IsChecked()
		}
		// Pre-gate in the UI: a Critical risk without the opt-in must not even
		// attempt the install (the core also refuses -- belt and suspenders).
		if !allowPrivileged {
			p.errMsg = "This module requests privileged/root-equivalent access. Check '" +
				allowPrivilegedLabel + "' to proceed, or pick a different module."
			p.mu.Unlock()
			p.render()
			return
		}
	}

	p.busy = true
	p.busyMsg = "Installing module..."
	p.errMsg = ""
	p.mu.Unlock()
	p.render()

	go func() {
		var name string
		var err error
		if p.cb.Install != nil {
			name, err = p.cb.Install(src, overrides, allowPrivileged)
		} else {
			err = fmt.Errorf("module install is not available")
		}

		p.mu.Lock()
		p.busy = false
		p.busyMsg = ""
		if err != nil {
			p.errMsg = err.Error()
		} else {
			p.phase = phaseDone
			p.message = fmt.Sprintf("Installed %s. Start it with: citadel run %s", name, name)
		}
		p.mu.Unlock()
		p.queueRender()
	}()
}

// reset returns the flow to phase 1.
func (p *ModulePage) reset() {
	p.mu.Lock()
	p.phase = phaseSource
	p.source = ""
	p.resolved = ModuleResolveResult{}
	p.errMsg = ""
	p.message = ""
	p.mu.Unlock()
	p.buildSourceForm()
	p.render()
}

// queueRender redraws on the UI goroutine if the page is live.
func (p *ModulePage) queueRender() {
	if p.app == nil {
		return
	}
	p.app.QueueUpdateDraw(func() { p.render() })
}

// render redraws the status pane from current state.
func (p *ModulePage) render() {
	if p.status == nil {
		return
	}
	p.mu.Lock()
	phase := p.phase
	res := p.resolved
	busy := p.busy
	busyMsg := p.busyMsg
	message := p.message
	errMsg := p.errMsg
	p.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("\n [yellow::b]Install a service module[-:-:-]\n\n")
	sb.WriteString(" [gray]Pick a known module on the left (Enter), or type a[-]\n")
	sb.WriteString(" [gray]source in the form. Tab moves list ↔ form ↔ tabs.[-]\n\n")
	sb.WriteString(" Install a module from any standardized repo.\n")
	sb.WriteString(" [gray]A module repo self-describes via citadel/service.yaml[-]\n")
	sb.WriteString(" [gray]+ citadel/compose.yml. Sources:[-]\n")
	sb.WriteString("   [aqua]vllm[-]                  [gray]catalog name[-]\n")
	sb.WriteString("   [aqua]owner/repo[-]            [gray]GitHub shorthand[-]\n")
	sb.WriteString("   [aqua]owner/repo@v1.2.0[-]     [gray]pinned ref[-]\n")
	sb.WriteString("   [aqua]https://git…/repo.git[-] [gray]full git URL[-]\n\n")
	sb.WriteString(" [gray]Private repos need this node to have git[-]\n")
	sb.WriteString(" [gray]credentials (GITHUB_TOKEN / SSH key / helper).[-]\n")

	if phase == phaseConfig && res.Name != "" {
		sb.WriteString("\n [white::b]Resolved[-:-:-]\n")
		sb.WriteString(fmt.Sprintf("   Name:  [aqua]%s[-]\n", res.Name))
		if res.Image != "" {
			sb.WriteString(fmt.Sprintf("   Image: [aqua]%s[-]\n", res.Image))
		}

		// Trust banner.
		if res.Trusted {
			sb.WriteString("   Source: [green]✓ trusted[-]\n")
		} else {
			sb.WriteString("   Source: [yellow]⚠ UNTRUSTED[-]\n")
			sb.WriteString("   [yellow]Installs & runs an arbitrary container with[-]\n")
			sb.WriteString("   [yellow]Docker-level (host root) access on this node.[-]\n")
		}

		// Risk findings.
		if len(res.Risks) > 0 {
			sb.WriteString("\n   [white::b]Compose risks[-:-:-]\n")
			for _, r := range res.Risks {
				if r.Critical {
					sb.WriteString(fmt.Sprintf("   [red]CRITICAL[-] %s\n", tview.Escape(r.Directive)))
				} else {
					sb.WriteString(fmt.Sprintf("   [yellow]HIGH[-] %s\n", tview.Escape(r.Directive)))
				}
			}
			if res.HasCriticalRisk {
				sb.WriteString(fmt.Sprintf("   [red]Check '%s' to proceed.[-]\n", allowPrivilegedLabel))
			}
		}

		if len(res.Config) > 0 {
			sb.WriteString("\n   [gray]Fill the config fields, then Install.[-]\n")
			sb.WriteString("   [gray]* = required[-]\n")
		} else {
			sb.WriteString("\n   [gray]No config required. Press Install.[-]\n")
		}
	}

	if busy {
		sb.WriteString(fmt.Sprintf("\n   [yellow]⏳ %s[-]\n", tview.Escape(busyMsg)))
	}
	if message != "" && !busy {
		sb.WriteString(fmt.Sprintf("\n   [green]%s[-]\n", tview.Escape(message)))
	}
	if errMsg != "" && !busy {
		sb.WriteString(fmt.Sprintf("\n   [red]%s[-]\n", tview.Escape(errMsg)))
	}

	p.status.SetText(sb.String())
}
