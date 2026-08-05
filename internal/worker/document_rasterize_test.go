package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// buildPDF writes a valid multi-page PDF with a real cross-reference table, so
// the render tests exercise poppler on a document it accepts without falling
// back to xref reconstruction. Every page carries visible text, which is what
// makes a rendered PNG non-blank and therefore worth measuring.
func buildPDF(pages int) []byte {
	var objects []string
	kids := make([]string, 0, pages)
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+i))
	}
	objects = append(objects,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages),
	)
	contentsStart := 3 + pages
	for i := range pages {
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 216 144] /Contents %d 0 R "+
				"/Resources << /Font << /F1 %d 0 R >> >> >>",
			contentsStart+i, contentsStart+pages))
	}
	for i := range pages {
		stream := fmt.Sprintf("BT /F1 18 Tf 18 64 Td (page %d) Tj ET", i+1)
		objects = append(objects, fmt.Sprintf(
			"<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	objects = append(objects, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects))
	for i, body := range objects {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xref)
	return buf.Bytes()
}

func rasterPayload(pdf []byte, pages ...any) map[string]any {
	list := make([]any, 0, len(pages))
	list = append(list, pages...)
	return map[string]any{
		"pdf_base64":         base64.StdEncoding.EncodeToString(pdf),
		"pages":              list,
		"dpi":                float64(200),
		"format":             "png",
		"max_bytes_per_page": float64(10 << 20),
	}
}

// stubRenderer returns a handler whose render step yields fixed bytes, so the
// validation and assembly paths are testable without poppler installed.
func stubRenderer(t *testing.T, img []byte) *DocumentRasterizeHandler {
	t.Helper()
	return NewDocumentRasterizeHandler(DocumentRasterizeConfig{
		LookRenderer: func() (string, error) { return "/usr/bin/pdftoppm", nil },
		RenderPage: func(ctx context.Context, bin string, pdf []byte, page, dpi int) ([]byte, error) {
			return img, nil
		},
	})
}

func runRaster(t *testing.T, h *DocumentRasterizeHandler, payload map[string]any) *JobResult {
	t.Helper()
	res, err := h.Execute(context.Background(), &Job{Type: JobTypeDocumentRasterize, Payload: payload}, nil)
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	if res == nil {
		t.Fatal("Execute returned no result")
	}
	return res
}

func TestDocumentRasterizeCanHandle(t *testing.T) {
	h := NewDocumentRasterizeHandler(DocumentRasterizeConfig{})
	if !h.CanHandle(JobTypeDocumentRasterize) {
		t.Fatalf("handler does not claim %q", JobTypeDocumentRasterize)
	}
	if h.CanHandle(JobTypeWebFetch) {
		t.Fatal("handler claims a job type it does not implement")
	}
}

// The job type has to be in allKnownJobTypes or the unsupported-type failure
// reports a node that supports it as not supporting it.
func TestDocumentRasterizeIsAKnownJobType(t *testing.T) {
	for _, jt := range allKnownJobTypes {
		if jt == JobTypeDocumentRasterize {
			return
		}
	}
	t.Fatalf("%q is missing from allKnownJobTypes", JobTypeDocumentRasterize)
}

func TestDocumentRasterizeReturnsOneEntryPerRequestedPage(t *testing.T) {
	h := stubRenderer(t, []byte("\x89PNG\r\n\x1a\nrendered"))
	res := runRaster(t, h, rasterPayload(buildPDF(3), float64(1), float64(3)))
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success (%v)", res.Status, res.Error)
	}

	entries, ok := res.Output["pages"].([]map[string]any)
	if !ok {
		t.Fatalf("pages is %T, want a list the caller can read", res.Output["pages"])
	}
	if len(entries) != 2 {
		t.Fatalf("got %d page(s), want 2", len(entries))
	}
	for i, want := range []int{1, 3} {
		// The caller type-checks the page number as an integer, so a float here
		// would fail validation on the other side of the wire.
		page, isInt := entries[i]["page"].(int)
		if !isInt || page != want {
			t.Fatalf("page[%d] = %v (%T), want int %d", i, entries[i]["page"], entries[i]["page"], want)
		}
		if entries[i]["mime"] != rasterizeMimePNG {
			t.Fatalf("page %d mime = %v, want %q", want, entries[i]["mime"], rasterizeMimePNG)
		}
		if entries[i]["image_base64"] == "" {
			t.Fatalf("page %d carries no image", want)
		}
	}
	if res.Output["renderer"] != rasterizeRenderer {
		t.Fatalf("renderer = %v, want %q", res.Output["renderer"], rasterizeRenderer)
	}
}

func TestDocumentRasterizeSortsAndDeduplicatesPages(t *testing.T) {
	calls := 0
	h := NewDocumentRasterizeHandler(DocumentRasterizeConfig{
		LookRenderer: func() (string, error) { return "pdftoppm", nil },
		RenderPage: func(ctx context.Context, bin string, pdf []byte, page, dpi int) ([]byte, error) {
			calls++
			return []byte("img"), nil
		},
	})
	res := runRaster(t, h, rasterPayload(buildPDF(5), float64(4), float64(2), float64(4)))
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success (%v)", res.Status, res.Error)
	}
	if calls != 2 {
		t.Fatalf("rendered %d time(s), want 2: a repeated page must not be rendered twice", calls)
	}
	entries := res.Output["pages"].([]map[string]any)
	if entries[0]["page"] != 2 || entries[1]["page"] != 4 {
		t.Fatalf("pages = %v, %v; want ascending 2, 4", entries[0]["page"], entries[1]["page"])
	}
}

// A node without poppler must say so in a way an operator can act on, rather
// than hanging or returning an empty page list that reads as a blank document.
func TestDocumentRasterizeWithoutARendererFailsWithAnInstallableMessage(t *testing.T) {
	h := NewDocumentRasterizeHandler(DocumentRasterizeConfig{
		LookRenderer: func() (string, error) { return "", exec.ErrNotFound },
		RenderPage: func(ctx context.Context, bin string, pdf []byte, page, dpi int) ([]byte, error) {
			t.Fatal("render must not be attempted without a renderer")
			return nil, nil
		},
	})
	res := runRaster(t, h, rasterPayload(buildPDF(1), float64(1)))
	if res.Status != JobStatusSuccess {
		msg := res.Error.Error()
		for _, want := range []string{rasterizeRenderer, rasterizePackage} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q does not name %q", msg, want)
			}
		}
		return
	}
	t.Fatal("a node with no renderer reported success")
}

// Every rejection is terminal. A retry re-runs the same bytes through the same
// missing or unhappy renderer and reaches the same place.
func TestDocumentRasterizeFailuresAreTerminal(t *testing.T) {
	cases := map[string]map[string]any{
		"no pdf":            {"pages": []any{float64(1)}},
		"undecodable pdf":   {"pdf_base64": "not base64 !!", "pages": []any{float64(1)}},
		"not a pdf":         {"pdf_base64": base64.StdEncoding.EncodeToString([]byte("GIF89a")), "pages": []any{float64(1)}},
		"no pages":          {"pdf_base64": base64.StdEncoding.EncodeToString(buildPDF(1)), "pages": []any{}},
		"page zero":         {"pdf_base64": base64.StdEncoding.EncodeToString(buildPDF(1)), "pages": []any{float64(0)}},
		"unsupported dpi":   {"pdf_base64": base64.StdEncoding.EncodeToString(buildPDF(1)), "pages": []any{float64(1)}, "dpi": float64(5000)},
		"wrong out format":  {"pdf_base64": base64.StdEncoding.EncodeToString(buildPDF(1)), "pages": []any{float64(1)}, "format": "jpeg"},
		"non numeric pages": {"pdf_base64": base64.StdEncoding.EncodeToString(buildPDF(1)), "pages": []any{"three-ish"}},
	}
	h := stubRenderer(t, []byte("img"))
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			res := runRaster(t, h, payload)
			if res.Status != JobStatusFailure {
				t.Fatalf("status = %v, want failure", res.Status)
			}
			if res.Error == nil || res.Error.Error() == "" {
				t.Fatal("a failure carried no reason")
			}
		})
	}
}

func TestDocumentRasterizeRejectsAnOversizedDocument(t *testing.T) {
	oversized := append(buildPDF(1), bytes.Repeat([]byte("x"), rasterizeMaxPDFBytes)...)
	res := runRaster(t, stubRenderer(t, []byte("img")), rasterPayload(oversized, float64(1)))
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure on a document over the render limit", res.Status)
	}
}

func TestDocumentRasterizeRejectsTooManyPages(t *testing.T) {
	pages := make([]any, rasterizeMaxPages+1)
	for i := range pages {
		pages[i] = float64(i + 1)
	}
	res := runRaster(t, stubRenderer(t, []byte("img")), rasterPayload(buildPDF(2), pages...))
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure past the per-job page limit", res.Status)
	}
}

// An image over the caller's per-page ceiling is a failure, never a silently
// dropped page: the caller validates that every requested page came back, so
// dropping one would surface as a confusing "missing page" instead of "lower
// the dpi".
func TestDocumentRasterizeRejectsAPageOverTheCallerCeiling(t *testing.T) {
	h := stubRenderer(t, bytes.Repeat([]byte("x"), 5000))
	payload := rasterPayload(buildPDF(1), float64(1))
	payload["max_bytes_per_page"] = float64(1000)
	res := runRaster(t, h, payload)
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure on an oversized page", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "dpi") {
		t.Fatalf("error %q does not tell the caller what to change", res.Error)
	}
}

// A caller cannot raise the per-page ceiling above the node's own.
func TestDocumentRasterizeClampsTheCallerCeiling(t *testing.T) {
	payload := rasterPayload(buildPDF(1), float64(1))
	payload["max_bytes_per_page"] = float64(rasterizeMaxBytesPerPage * 4)
	req, err := parseRasterizeRequest(payload)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if req.MaxBytesPerPage != rasterizeMaxBytesPerPage {
		t.Fatalf("MaxBytesPerPage = %d, want it clamped to %d", req.MaxBytesPerPage, rasterizeMaxBytesPerPage)
	}
}

func TestDocumentRasterizeDefaultsTheResolution(t *testing.T) {
	payload := rasterPayload(buildPDF(1), float64(1))
	delete(payload, "dpi")
	req, err := parseRasterizeRequest(payload)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if req.DPI != rasterizeDefaultDPI {
		t.Fatalf("DPI = %d, want the %d default", req.DPI, rasterizeDefaultDPI)
	}
}

// An empty render is a failure. A zero-byte "image" would decode to nothing on
// the caller's side and read as a page with no content.
func TestDocumentRasterizeRejectsAnEmptyRender(t *testing.T) {
	res := runRaster(t, stubRenderer(t, nil), rasterPayload(buildPDF(1), float64(1)))
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure on an empty render", res.Status)
	}
}

func TestDocumentRasterizeStopsWhenTheJobDeadlinePasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := NewDocumentRasterizeHandler(DocumentRasterizeConfig{
		LookRenderer: func() (string, error) { return "pdftoppm", nil },
		RenderPage: func(ctx context.Context, bin string, pdf []byte, page, dpi int) ([]byte, error) {
			cancel() // the deadline passes while the first page renders
			return []byte("img"), nil
		},
	})
	res, err := h.Execute(ctx, &Job{
		Type:    JobTypeDocumentRasterize,
		Payload: rasterPayload(buildPDF(2), float64(1), float64(2)),
	}, nil)
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure once the deadline passed", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "of 2 page(s)") {
		t.Fatalf("error %q does not say how far it got", res.Error)
	}
}

// The live path: real poppler, a real PDF, real PNG bytes back. Skipped rather
// than failed where poppler is absent, which is exactly the condition the
// capability error above covers.
func TestDocumentRasterizeRendersRealPagesWithPoppler(t *testing.T) {
	if _, err := exec.LookPath(rasterizeRenderer); err != nil {
		t.Skipf("%s is not installed", rasterizeRenderer)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h := NewDocumentRasterizeHandler(DocumentRasterizeConfig{})
	res, err := h.Execute(ctx, &Job{
		Type:    JobTypeDocumentRasterize,
		Payload: rasterPayload(buildPDF(3), float64(1), float64(3)),
	}, nil)
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %v, want success (%v)", res.Status, res.Error)
	}

	entries := res.Output["pages"].([]map[string]any)
	if len(entries) != 2 {
		t.Fatalf("got %d page(s), want 2", len(entries))
	}
	for _, entry := range entries {
		raw, decErr := base64.StdEncoding.DecodeString(entry["image_base64"].(string))
		if decErr != nil {
			t.Fatalf("page %v did not decode: %v", entry["page"], decErr)
		}
		if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatalf("page %v is not a PNG", entry["page"])
		}
		if len(raw) < 1000 {
			t.Fatalf("page %v rendered to %d bytes, too small to be a real page", entry["page"], len(raw))
		}
	}
}

// A page past the end of the document is named, not swallowed. poppler exits
// non-zero and its first diagnostic is what the caller shows.
func TestDocumentRasterizeNamesAPagePastTheEndOfTheDocument(t *testing.T) {
	if _, err := exec.LookPath(rasterizeRenderer); err != nil {
		t.Skipf("%s is not installed", rasterizeRenderer)
	}
	res := runRaster(t, NewDocumentRasterizeHandler(DocumentRasterizeConfig{}),
		rasterPayload(buildPDF(1), float64(9)))
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %v, want failure for a page the document does not have", res.Status)
	}
	if !strings.Contains(res.Error.Error(), "page 9") {
		t.Fatalf("error %q does not name the page", res.Error)
	}
}
