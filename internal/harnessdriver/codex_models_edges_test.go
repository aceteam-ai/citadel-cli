package harnessdriver

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestCodexModelsRejectMalformedAmbiguousAndOptionalVisibility(t *testing.T) {
	good := `{"id":"list-1","result":{"data":[{"id":"catalog-a","model":"fixture-a","hidden":false}],"nextCursor":null}}`
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"id":"list-1","result":null}`,
		strings.Replace(good, `"list-1"`, `1`, 1),
		strings.Replace(good, `"id":"list-1"`, `"id":"list-1","id":"list-1"`, 1),
		strings.Replace(good, `"hidden":false`, `"hidden":null`, 1),
		strings.Replace(good, `"hidden":false`, `"hidden":"false"`, 1),
		strings.Replace(good, `,"hidden":false`, ``, 1),
		strings.Replace(good, `"model":"fixture-a"`, `"model":""`, 1),
		strings.Replace(good, `"model":"fixture-a"`, `"model":"fixture with spaces"`, 1),
		strings.Replace(good, `"nextCursor":null`, `"nextCursor":""`, 1),
		strings.Replace(good, `,"nextCursor":null`, ``, 1),
		strings.Replace(good, `"nextCursor":null`, `"nextCursor":17`, 1),
		`{"id":"list-1","result":{"data":null,"nextCursor":null}}`,
		`{"id":"list-1","result":{"data":[null],"nextCursor":null}}`,
		`{"id":"list-1","result":{"data":[],"nextCursor":null},"error":null}`,
		`{"id":"list-1","result":{"data":[],"nextCursor":null},"error":{"message":"fixture-detail"}}`,
		`{"id":"list-1","result":{"data":[],"nextCursor":null},"method":"model/list"}`,
		`{"id":"list-1","result":{"data":[],"nextCursor":null},"params":{}}`,
		`{"id":"list-1","result":{"data":[],"nextCursor":null}} {}`,
	} {
		catalog, err := ParseCodexModelCatalog([]CodexModelPage{{RequestID: "list-1", Response: []byte(raw)}})
		if err != ErrCodexModelCatalog || catalog != nil || strings.Contains(fmt.Sprint(err), "fixture") {
			t.Fatalf("malformed catalog accepted or echoed: %v %v", catalog, err)
		}
	}
	for _, rows := range []string{
		`[{"id":"same","model":"fixture-a","hidden":false},{"id":"same","model":"fixture-b","hidden":false}]`,
		`[{"id":"catalog-a","model":"same","hidden":false},{"id":"catalog-b","model":"same","hidden":true}]`,
	} {
		if catalog, err := ParseCodexModelCatalog([]CodexModelPage{modelPage("list-1", "", "", rows)}); err == nil || catalog != nil {
			t.Fatal("ambiguous model mapping accepted", catalog, err)
		}
	}
}

func TestCodexModelsPaginationIsCompleteBoundedAndCorrelated(t *testing.T) {
	a := modelPage("list-1", "", "cursor-a", `[]`)
	b := modelPage("list-2", "cursor-a", "", `[]`)
	for _, pages := range [][]CodexModelPage{
		nil,
		{modelPage("list-1", "initial-cursor", "", `[]`)},
		{a},
		{a, modelPage("list-2", "cursor-a", "cursor-a", `[]`)},
		{a, modelPage("list-1", "cursor-a", "", `[]`)},
		{b, a},
		{a, b, modelPage("list-3", "", "", `[]`)},
		{a, modelPage("list-2", "cursor-a", "cursor-b", `[]`), modelPage("list-3", "cursor-b", "cursor-a", `[]`)},
	} {
		if catalog, err := ParseCodexModelCatalog(pages); err == nil || catalog != nil {
			t.Fatal("pagination accepted out-of-context pages", catalog, err)
		}
	}
	if _, err := ParseCodexModelCatalog(make([]CodexModelPage, maxCodexModelPages+1)); err != ErrCodexModelLimit {
		t.Fatal("page bound ignored", err)
	}
	complete := make([]CodexModelPage, maxCodexModelPages)
	cursor := ""
	for i := range complete {
		next := fmt.Sprintf("cursor-%d", i)
		if i == len(complete)-1 {
			next = ""
		}
		complete[i] = modelPage(fmt.Sprintf("list-%d", i), cursor, next, `[]`)
		cursor = next
	}
	if _, err := ParseCodexModelCatalog(complete); err != nil {
		t.Fatal("exact page bound refused", err)
	}
	rows := make([]string, maxCodexModels)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":"catalog-%d","model":"fixture-%d","hidden":false}`, i, i)
	}
	first := modelPage("list-1", "", "cursor-a", "["+strings.Join(rows[:64], ",")+"]")
	second := modelPage("list-2", "cursor-a", "", "["+strings.Join(rows[64:], ",")+"]")
	if catalog, err := ParseCodexModelCatalog([]CodexModelPage{first, second}); err != nil || catalog == nil {
		t.Fatal("exact model bound refused", err)
	}
	second = modelPage("list-2", "cursor-a", "", "["+strings.Join(rows[64:], ",")+`,{"id":"extra","model":"extra","hidden":true}]`)
	if catalog, err := ParseCodexModelCatalog([]CodexModelPage{first, second}); err != ErrCodexModelLimit || catalog != nil {
		t.Fatal("hidden row bypassed aggregate bound", catalog, err)
	}
	duplicate := modelPage("list-2", "cursor-a", "", "["+rows[0]+"]")
	if catalog, err := ParseCodexModelCatalog([]CodexModelPage{first, duplicate}); err == nil || catalog != nil {
		t.Fatal("cross-page row ambiguity accepted", catalog, err)
	}
}

func TestCodexModelsOmitUnusedMetadataWithoutDroppingCatalog(t *testing.T) {
	rows := `[{"id":"catalog-a","model":"fixture-a","hidden":false,"isDefault":"unprojected-shape","displayName":{"unexpected":"fixture-detail"},"supportedReasoningEfforts":"unprojected-shape","upgradeInfo":false}]`
	catalog, err := ParseCodexModelCatalog([]CodexModelPage{modelPage("list-1", "", "", rows)})
	if err != nil {
		t.Fatal("unused optional metadata discarded the required response", err)
	}
	models, err := catalog.Models()
	if err != nil || !reflect.DeepEqual(models.IDs, []string{"fixture-a"}) {
		t.Fatal(models, err)
	}
	if _, err := catalog.Select(""); err != ErrCodexModelSelection {
		t.Fatal("default metadata caused implicit selection")
	}
}

func TestCodexModelsOwnDataAndKeepDiagnosticsContentFree(t *testing.T) {
	page := modelPage("list-1", "", "", `[{"id":"catalog-a","model":"fixture-content","hidden":false}]`)
	catalog, err := ParseCodexModelCatalog([]CodexModelPage{page})
	if err != nil {
		t.Fatal(err)
	}
	for i := range page.Response {
		page.Response[i] = 'x'
	}
	models, _ := catalog.Models()
	models.IDs[0] = "changed"
	selected, err := catalog.Select("fixture-content")
	if err != nil || selected != "fixture-content" {
		t.Fatal("caller mutation changed catalog", err)
	}
	for _, value := range []any{page, catalog, *catalog} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(format, value), "fixture-content") || strings.Contains(fmt.Sprintf(format, value), "list-1") {
				t.Fatal("diagnostic exposed content")
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil || string(encoded) != "{}" {
			t.Fatal("catalog data serialized implicitly", string(encoded), err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, err := catalog.Models()
			if err != nil {
				t.Error(err)
				return
			}
			models.IDs[0] = "changed"
			if got, err := catalog.Select("fixture-content"); err != nil || got != "fixture-content" {
				t.Error("concurrent catalog mutation")
			}
		}()
	}
	wg.Wait()
}

func TestCodexModelsBoundsEmptyCatalogAndInvalidValues(t *testing.T) {
	for _, response := range [][]byte{
		[]byte(strings.Repeat(" ", maxModelPageBytes+1)),
		append([]byte(`{"invalid":"`), 0xff),
		[]byte(`{"nested":` + strings.Repeat("[", 18) + `0` + strings.Repeat("]", 18) + `}`),
	} {
		if catalog, err := ParseCodexModelCatalog([]CodexModelPage{{RequestID: "list-1", Response: response}}); err == nil || catalog != nil {
			t.Fatal("unbounded or invalid input accepted", catalog, err)
		}
	}
	for _, value := range []string{"", strings.Repeat("a", 129), "line\nfeed", "\ufffd"} {
		page := modelPage(value, "", "", `[]`)
		if _, err := ParseCodexModelCatalog([]CodexModelPage{page}); err == nil {
			t.Fatal("invalid request identity accepted")
		}
	}
	for _, value := range []string{strings.Repeat("a", 1025), "line\nfeed", "\ufffd"} {
		if _, err := ParseCodexModelCatalog([]CodexModelPage{modelPage("list-1", "", value, `[]`)}); err != ErrCodexModelCatalog {
			t.Fatal("invalid cursor accepted", err)
		}
	}
	for _, row := range []string{
		`{"id":"` + strings.Repeat("a", 257) + `","model":"fixture-a","hidden":false}`,
		`{"id":"catalog-a","model":"` + strings.Repeat("a", 129) + `","hidden":false}`,
		`{"id":"catalog-a","model":"fixture-a\n","hidden":false}`,
	} {
		if _, err := ParseCodexModelCatalog([]CodexModelPage{modelPage("list-1", "", "", "["+row+"]")}); err != ErrCodexModelCatalog {
			t.Fatal("invalid model identity accepted", err)
		}
	}
	for _, rows := range []string{`[]`, `[{"id":"catalog-a","model":"fixture-hidden","hidden":true}]`} {
		catalog, err := ParseCodexModelCatalog([]CodexModelPage{modelPage("list-1", "", "", rows)})
		if err != nil {
			t.Fatal(err)
		}
		models, err := catalog.Models()
		if err != nil || models.IDs == nil || len(models.IDs) != 0 {
			t.Fatal("empty catalog became null or error", models, err)
		}
		if _, err := catalog.Select("fixture-hidden"); err != ErrCodexModelSelection {
			t.Fatal("empty/hidden catalog selected a model")
		}
	}
	var absent *CodexModelCatalog
	for _, catalog := range []*CodexModelCatalog{absent, {}} {
		if _, err := catalog.Models(); err != ErrCodexModelCatalog {
			t.Fatal("zero catalog reported models")
		}
		if _, err := catalog.Select("fixture-a"); err != ErrCodexModelSelection {
			t.Fatal("zero catalog selected model")
		}
	}
}

func TestCodexModelsGoldenPagesValidateCandidatePayload(t *testing.T) {
	fixture, err := os.ReadFile("testdata/codex-models.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	if len(lines) != 2 {
		t.Fatal("unexpected fixture page count")
	}
	pages := []CodexModelPage{{RequestID: "list-1", Response: []byte(lines[0])}, {RequestID: "list-2", Cursor: "page-two", Response: []byte(lines[1])}}
	catalog, err := ParseCodexModelCatalog(pages)
	if err != nil {
		t.Fatal(err)
	}
	models, err := catalog.Models()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(models)
	if err != nil || string(encoded) != `{"ids":["fixture-a","fixture-b"]}` {
		t.Fatal("fixture projection changed", string(encoded), err)
	}
	event := sampleEvent(1)
	event.Type, event.Data = "models.reported", encoded
	if err := event.Validate(); err != nil {
		t.Fatal("model projection drifted from candidate schema", err)
	}
}
