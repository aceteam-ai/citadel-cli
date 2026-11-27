package harnessdriver

import (
	"encoding/json"
	"reflect"
	"testing"
)

func modelPage(id, cursor, next string, models string) CodexModelPage {
	nextJSON := "null"
	if next != "" {
		b, _ := json.Marshal(next)
		nextJSON = string(b)
	}
	idJSON, _ := json.Marshal(id)
	return CodexModelPage{RequestID: id, Cursor: cursor, Response: []byte(`{"id":` + string(idJSON) + `,"result":{"data":` + models + `,"nextCursor":` + nextJSON + `}}`)}
}

func TestCodexModelsRequireCompleteCatalogAndExplicitSelection(t *testing.T) {
	pages := []CodexModelPage{
		modelPage("list-1", "", "page-two", `[{"id":"catalog-a","model":"fixture-a","hidden":false,"isDefault":true}]`),
		modelPage("list-2", "page-two", "", `[{"id":"catalog-b","model":"fixture-b","hidden":false},{"id":"catalog-c","model":"fixture-hidden","hidden":true}]`),
	}
	catalog, err := ParseCodexModelCatalog(pages)
	if err != nil {
		t.Fatal(err)
	}
	models, err := catalog.Models()
	if err != nil || !reflect.DeepEqual(models.IDs, []string{"fixture-a", "fixture-b"}) {
		t.Fatal(models, err)
	}
	if selected, err := catalog.Select("fixture-b"); err != nil || selected != "fixture-b" {
		t.Fatal(selected, err)
	}
	for _, requested := range []string{"", "catalog-a", "configured-only", "fixture-hidden"} {
		if _, err := catalog.Select(requested); err != ErrCodexModelSelection {
			t.Fatal("implicit or absent selection accepted", requested, err)
		}
	}
}

func TestCodexModelsRejectIncompleteAndUncorrelatedPages(t *testing.T) {
	first := modelPage("list-1", "", "page-two", `[{"id":"catalog-a","model":"fixture-a","hidden":false}]`)
	if catalog, err := ParseCodexModelCatalog([]CodexModelPage{first}); err == nil || catalog != nil {
		t.Fatal("partial catalog escaped", catalog, err)
	}
	second := modelPage("list-2", "wrong-cursor", "", `[]`)
	if catalog, err := ParseCodexModelCatalog([]CodexModelPage{first, second}); err == nil || catalog != nil {
		t.Fatal("foreign cursor accepted", catalog, err)
	}
	first.RequestID = "foreign-id"
	if catalog, err := ParseCodexModelCatalog([]CodexModelPage{first}); err == nil || catalog != nil {
		t.Fatal("foreign response accepted", catalog, err)
	}
}
