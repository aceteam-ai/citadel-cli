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

// ModuleResolveResult is what Resolve returns: the resolved module name, the
// container image (from the module's own compose, display-only), and the list of
// config fields the form should collect.
type ModuleResolveResult struct {
	Name   string
	Image  string
	Config []ConfigField
}

// ModuleInstallCallbacks holds the hooks for the "Install module" page. They are
// wired from cmd so the page stays free of catalog/network/docker imports.
//
// Resolve clones/updates the source repo (or loads a catalog name) and returns
// the module name + required config; it may block on the network, so the page
// calls it off the UI goroutine. Install performs the actual install with the
// collected config passed as overrides (the non-interactive installer path, so
// no stdin is read).
type ModuleInstallCallbacks struct {
	Resolve func(source string) (ModuleResolveResult, error)
	Install func(source string, overrides map[string]string) (installedName string, err error)
}

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
	root   *tview.Flex
	form   *tview.Form
	status *tview.TextView

	// State
	mu       sync.Mutex
	phase    modulePhase
	source   string
	resolved ModuleResolveResult
	busy     bool
	busyMsg  string
	message  string // last success/info message
	errMsg   string // last error message
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
		AddItem(p.form, 0, 1, true).
		AddItem(p.status, 0, 1, false)

	p.buildSourceForm()
	p.render()
	return p.root
}

// OnActivate implements Page.
func (p *ModulePage) OnActivate() {
	if p.app != nil && p.form != nil {
		p.app.SetFocus(p.form)
	}
}

// OnDeactivate implements Page.
func (p *ModulePage) OnDeactivate() {}

// HandleInput implements Page. The form consumes most keys; 'r' resets the flow.
func (p *ModulePage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	// Let the form (text inputs, buttons) handle everything; the page does not
	// steal keys so the user can type a source/config freely.
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

	p.busy = true
	p.busyMsg = "Installing module..."
	p.errMsg = ""
	p.mu.Unlock()
	p.render()

	go func() {
		var name string
		var err error
		if p.cb.Install != nil {
			name, err = p.cb.Install(src, overrides)
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
		if len(res.Config) > 0 {
			sb.WriteString("   [gray]Fill the config fields, then Install.[-]\n")
			sb.WriteString("   [gray]* = required[-]\n")
		} else {
			sb.WriteString("   [gray]No config required. Press Install.[-]\n")
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
