package controlcenter

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestBuiltinServiceDetailsHaveNoInternalIssueReferences(t *testing.T) {
	cc, _ := newTestControlCenterWithPermissions()
	cc.app = tview.NewApplication()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	cc.app.SetScreen(screen)
	screen.SetSize(220, 50)
	cc.showBuiltinServicesModal()
	table, ok := cc.app.GetFocus().(*tview.Table)
	if !ok {
		t.Fatal("built-in services table is not focused")
	}
	ref := regexp.MustCompile(`(?i)(?:aceteam|citadel(?:-cli)?)\s*#\d+`)
	for row := 0; row < table.GetRowCount(); row++ {
		table.Select(row, 0)
		cc.app.ForceDraw()
		var rendered strings.Builder
		w, h := screen.Size()
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, _, _, _ := screen.GetContent(x, y)
				rendered.WriteRune(r)
			}
			rendered.WriteByte('\n')
		}
		text := rendered.String()
		if ref.MatchString(text) {
			t.Errorf("service row %d leaks reference: %s", row, text)
		}
		if row == 0 && !strings.Contains(text, "passcode-gated") {
			t.Fatalf("passcode guidance not rendered: %s", text)
		}
	}
}
