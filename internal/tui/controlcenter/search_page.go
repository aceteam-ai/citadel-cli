package controlcenter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/rag"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// SearchPage implements the Page interface for the local semantic Search tab
// (citadel-cli#617-619). It drives internal/rag IN-PROCESS over the
// authorized-roots allowlist: a query box, a results list (path + snippet), and
// a small roots-management affordance. Every rag call (which hits the local TEI
// service and can block) runs on a background goroutine; all UI mutations go
// back through app.QueueUpdateDraw so the tview event loop is never blocked.
type SearchPage struct {
	app *tview.Application

	// UI
	root      *tview.Flex
	header    *tview.TextView
	input     *tview.InputField
	results   *tview.List
	addRoot   *tview.InputField
	statusBar *tview.TextView

	// searchSeq guards against a slow earlier query overwriting a newer one's
	// results: each search increments it and only the latest applies.
	mu        sync.Mutex
	searchSeq int
}

// NewSearchPage constructs the Search page. Roots and the rag service are
// resolved lazily (per action) from the on-disk allowlist so authorizing a root
// in another surface is picked up without restarting the TUI.
func NewSearchPage() *SearchPage { return &SearchPage{} }

// Name implements Page.
func (p *SearchPage) Name() string { return "search" }

// Title implements Page.
func (p *SearchPage) Title() string { return "Search" }

// service builds a fresh roots-mode rag.Service from the current allowlist.
func (p *SearchPage) service() (*rag.Service, []string) {
	roots := config.LoadRoots(platform.ConfigDir()).Roots
	return rag.NewWithRoots(roots, rag.NodeWorkspaceDir(), ""), roots
}

// Build implements Page.
func (p *SearchPage) Build(app *tview.Application) tview.Primitive {
	p.app = app

	p.header = tview.NewTextView().SetDynamicColors(true)
	p.header.SetBorder(true).SetTitle(" Local Semantic Search ").SetTitleAlign(tview.AlignLeft)

	p.input = tview.NewInputField()
	p.input.SetLabel("Search: ")
	p.input.SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
	p.input.SetLabelColor(tcell.ColorGreen)
	p.input.SetPlaceholder("Type a query and press Enter")
	p.input.SetPlaceholderTextColor(tcell.ColorDimGray)
	p.input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			p.runSearch(strings.TrimSpace(p.input.GetText()))
		}
	})

	p.results = tview.NewList().ShowSecondaryText(true)
	p.results.SetBorder(true).SetTitle(" Results ").SetTitleAlign(tview.AlignLeft)

	p.addRoot = tview.NewInputField()
	p.addRoot.SetLabel("Authorize root: ")
	p.addRoot.SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
	p.addRoot.SetLabelColor(tcell.ColorYellow)
	p.addRoot.SetPlaceholder("Path to a directory, then Enter (index runs after)")
	p.addRoot.SetPlaceholderTextColor(tcell.ColorDimGray)
	p.addRoot.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			p.authorizeRoot(strings.TrimSpace(p.addRoot.GetText()))
		}
	})

	p.statusBar = tview.NewTextView().SetDynamicColors(true)

	p.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.header, 4, 0, false).
		AddItem(p.input, 1, 0, true).
		AddItem(p.results, 0, 1, false).
		AddItem(p.statusBar, 1, 0, false).
		AddItem(p.addRoot, 1, 0, false)

	p.refreshHeader()
	return p.root
}

// OnActivate implements Page. Focuses the query input and refreshes the roots
// header (which may have changed since the tab was last viewed).
func (p *SearchPage) OnActivate() {
	p.refreshHeader()
	if p.app != nil && p.input != nil {
		p.app.SetFocus(p.input)
	}
}

// OnDeactivate implements Page.
func (p *SearchPage) OnDeactivate() {}

// HandleInput implements Page. Tab cycles focus query -> results -> add-root.
func (p *SearchPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	if event.Key() == tcell.KeyTAB {
		p.cycleFocus()
		return nil
	}
	return event
}

func (p *SearchPage) cycleFocus() {
	if p.app == nil {
		return
	}
	order := []tview.Primitive{p.input, p.results, p.addRoot}
	cur := p.app.GetFocus()
	next := 0
	for i, prim := range order {
		if prim == cur {
			next = (i + 1) % len(order)
			break
		}
	}
	p.app.SetFocus(order[next])
}

// refreshHeader renders the authorized roots and model into the header. Safe to
// call from the main goroutine (it writes the primitive directly).
func (p *SearchPage) refreshHeader() {
	roots := config.LoadRoots(platform.ConfigDir()).Roots
	var b strings.Builder
	if len(roots) == 0 {
		b.WriteString("[yellow]No authorized roots.[white] Add one below to index and search.\n")
	} else {
		b.WriteString(fmt.Sprintf("[green]%d authorized root(s):[white] %s\n", len(roots), strings.Join(roots, "  ")))
	}
	b.WriteString("[gray]Model: gte-multilingual-base   Tab: switch field   Enter: run[white]")
	p.header.SetText(b.String())
}

func (p *SearchPage) setStatus(format string, a ...any) {
	if p.app == nil {
		return
	}
	p.app.QueueUpdateDraw(func() {
		p.statusBar.SetText(fmt.Sprintf(format, a...))
	})
}

// runSearch executes a query on a background goroutine and populates the results
// list via QueueUpdateDraw.
func (p *SearchPage) runSearch(query string) {
	if query == "" {
		return
	}
	p.mu.Lock()
	p.searchSeq++
	seq := p.searchSeq
	p.mu.Unlock()

	p.setStatus("[gray]Searching...[white]")
	go func() {
		svc, roots := p.service()
		if len(roots) == 0 {
			p.setStatus("[yellow]No authorized roots — add one below first.[white]")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := svc.Query(ctx, query, 20)

		p.mu.Lock()
		latest := seq == p.searchSeq
		p.mu.Unlock()
		if !latest {
			return // a newer search superseded this one
		}

		if p.app == nil {
			return
		}
		p.app.QueueUpdateDraw(func() {
			p.results.Clear()
			if err != nil {
				p.statusBar.SetText(fmt.Sprintf("[red]Search failed: %v[white]", err))
				return
			}
			if len(res.Hits) == 0 {
				p.statusBar.SetText("[yellow]No results. Have you run an index?[white]")
				return
			}
			for _, h := range res.Hits {
				primary := fmt.Sprintf("%.3f  %s", h.Score, h.Path)
				secondary := snippetOneLine(h.Text)
				p.results.AddItem(primary, secondary, 0, nil)
			}
			p.statusBar.SetText(fmt.Sprintf("[green]%d result(s)[white]", len(res.Hits)))
		})
	}()
}

// authorizeRoot adds a root to the allowlist, saves it, and kicks off an index
// of it — all on a background goroutine.
func (p *SearchPage) authorizeRoot(path string) {
	if path == "" {
		return
	}
	p.setStatus("[gray]Authorizing %s ...[white]", path)
	go func() {
		configDir := platform.ConfigDir()
		roots := config.LoadRoots(configDir)
		added, err := roots.Add(path)
		if err != nil {
			p.setStatus("[red]Cannot authorize %s: %v[white]", path, err)
			return
		}
		if added {
			if err := config.SaveRoots(configDir, roots); err != nil {
				p.setStatus("[red]Cannot save roots: %v[white]", err)
				return
			}
		}
		if p.app != nil {
			p.app.QueueUpdateDraw(func() {
				p.addRoot.SetText("")
				p.refreshHeader()
			})
		}
		// Index the newly-authorized tree so it is searchable. Best-effort.
		p.setStatus("[gray]Indexing %s ...[white]", path)
		svc := rag.NewWithRoots(roots.Roots, rag.NodeWorkspaceDir(), "")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := svc.Index(ctx, path, ""); err != nil {
			p.setStatus("[red]Indexing failed: %v[white]", err)
			return
		}
		p.setStatus("[green]Authorized and indexed %s[white]", path)
	}()
}

// snippetOneLine collapses a chunk's text to a single line for the list's
// secondary row.
func snippetOneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
