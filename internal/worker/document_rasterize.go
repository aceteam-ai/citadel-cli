// internal/worker/document_rasterize.go
//
// document_rasterize job handler (issue #675). Renders selected pages of a PDF
// to images on THIS node so a scanned document can reach an OCR model.
//
// # Why the node has to do this
//
// The OCR engines served on a node (the Unlimited-OCR vLLM server, for one)
// decode their image content part through PIL. A PDF posted as that part is
// refused before the model sees anything:
//
//	HTTP 400 {"error":{"message":"Failed to load image: cannot identify image
//	          file <_io.BytesIO object>","type":"BadRequestError","code":400}}
//
// So a page has to be a raster image before it is an inference request. Doing
// that render on the node rather than on the caller keeps the CPU cost on the
// hardware that already runs the model, and keeps the document's bytes on the
// operator's own machine.
//
// # Contract
//
// Request payload:
//
//	pdf_base64          string  the PDF bytes, base64
//	pages               []int   1-based page numbers to render, only these
//	dpi                 int     render resolution (default 200)
//	format              string  "png" (the only supported value)
//	max_bytes_per_page  int     per-image ceiling the caller will accept
//
// Result:
//
//	{"pages": [{"page": 3, "image_base64": "...", "mime": "image/png"}, ...],
//	 "dpi": 200, "format": "png", "renderer": "pdftoppm"}
//
// One entry per requested page, in the requested order. The caller validates
// strictly, so a page that could not be rendered fails the whole job with a
// named reason rather than being dropped from the list.
//
// # Renderer dependency
//
// citadel ships as a single static binary with no native runtime dependencies,
// and rendering a PDF is the first thing that needs one. Rather than assume it,
// the handler looks the renderer up at execution time and, when it is absent,
// fails with a terminal message naming the missing binary and the package that
// provides it. An operator gets an instruction, never a hang or an empty result.
//
// The renderer is poppler's pdftoppm, driven over stdin/stdout: the PDF is
// piped in and the PNG is read back out, so a customer document is never
// written to the node's disk.
//
// # Gating
//
// Unconditional, like WEB_FETCH. The input is bytes carried in the payload; the
// handler reaches no file, no credential, and no device on the node, so there is
// nothing for a per-node-stream gate to protect.
package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

const (
	// rasterizeRenderer is the poppler utility that does the rendering, and
	// rasterizePackage is what an operator installs to get it.
	rasterizeRenderer = "pdftoppm"
	rasterizePackage  = "poppler-utils"

	// rasterizeDefaultDPI matches the caller's default. 200 DPI keeps small
	// print legible to a vision model while a Letter page stays a few hundred
	// KB as PNG.
	rasterizeDefaultDPI = 200

	// rasterizeMinDPI / rasterizeMaxDPI bound what a caller may ask for. Below
	// the minimum nothing is legible; above the maximum one page can outgrow
	// any sane transport before the per-page cap catches it.
	rasterizeMinDPI = 36
	rasterizeMaxDPI = 600

	// rasterizeFormatPNG is the only supported output format. PNG is lossless:
	// JPEG ringing around glyph edges is exactly the noise an OCR model reads
	// as a wrong character.
	rasterizeFormatPNG = "png"
	rasterizeMimePNG   = "image/png"

	// rasterizeMaxPDFBytes bounds the decoded input. The caller refuses to send
	// a larger document; this is the node's own guard against a payload that
	// would render this worker unresponsive.
	rasterizeMaxPDFBytes = 16 << 20

	// rasterizeMaxBytesPerPage is the hard per-image ceiling. A caller may ask
	// for less, never for more.
	rasterizeMaxBytesPerPage = 10 << 20

	// rasterizeMaxTotalBytes bounds one job's whole result, independently of
	// how many pages were requested.
	rasterizeMaxTotalBytes = 32 << 20

	// rasterizeMaxPages bounds the pages one job may ask for. Each page is a
	// separate render process, and the caller's own page cap is far below this.
	rasterizeMaxPages = 64
)

// errRasterizerMissing reports that the node has no PDF renderer installed. It
// is terminal: retrying cannot install poppler.
var errRasterizerMissing = fmt.Errorf(
	"no PDF renderer on this node: %s not found in PATH (install %s, e.g. `apt-get install -y %s`)",
	rasterizeRenderer, rasterizePackage, rasterizePackage,
)

// RasterizeRequest is the parsed document_rasterize payload.
type RasterizeRequest struct {
	// PDF is the decoded document.
	PDF []byte
	// Pages are the 1-based page numbers to render, deduplicated and ordered.
	Pages []int
	// DPI is the render resolution.
	DPI int
	// MaxBytesPerPage is the per-image ceiling, never above
	// rasterizeMaxBytesPerPage.
	MaxBytesPerPage int
}

// DocumentRasterizeConfig configures a DocumentRasterizeHandler. Both edges are
// injectable so the handler's validation and assembly are unit-testable without
// poppler installed.
type DocumentRasterizeConfig struct {
	// LookRenderer resolves the renderer binary. Defaults to exec.LookPath for
	// rasterizeRenderer.
	LookRenderer func() (string, error)

	// RenderPage renders one 1-based page of pdf at dpi and returns PNG bytes.
	// Defaults to the pdftoppm pipe.
	RenderPage func(ctx context.Context, bin string, pdf []byte, page, dpi int) ([]byte, error)

	// Log reports progress. Nil is a no-op.
	Log func(format string, args ...any)
}

// DocumentRasterizeHandler processes document_rasterize jobs.
type DocumentRasterizeHandler struct {
	cfg DocumentRasterizeConfig
}

// NewDocumentRasterizeHandler constructs a document_rasterize handler with the
// live poppler edges, overridable through cfg.
func NewDocumentRasterizeHandler(cfg DocumentRasterizeConfig) *DocumentRasterizeHandler {
	if cfg.LookRenderer == nil {
		cfg.LookRenderer = func() (string, error) { return exec.LookPath(rasterizeRenderer) }
	}
	if cfg.RenderPage == nil {
		cfg.RenderPage = renderPageWithPdftoppm
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &DocumentRasterizeHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *DocumentRasterizeHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeDocumentRasterize
}

// Execute renders the requested pages and returns one image per page. Every
// failure is terminal: a malformed payload, an absent renderer, and a PDF
// poppler cannot read are all things a retry would reproduce exactly.
func (h *DocumentRasterizeHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	req, err := parseRasterizeRequest(job.Payload)
	if err != nil {
		return h.failure(fmt.Errorf("%s: %w", JobTypeDocumentRasterize, err)), nil
	}

	bin, lookErr := h.cfg.LookRenderer()
	if lookErr != nil {
		// Capability, not corruption. The message names the binary and the
		// package so the caller can tell an operator what to install.
		return h.failure(fmt.Errorf("%s: %w", JobTypeDocumentRasterize, errRasterizerMissing)), nil
	}

	h.cfg.Log("%s: rendering %d page(s) at %d DPI", JobTypeDocumentRasterize, len(req.Pages), req.DPI)

	rendered := make([]map[string]any, 0, len(req.Pages))
	total := 0
	for _, page := range req.Pages {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return h.failure(fmt.Errorf(
				"%s: ran out of time after %d of %d page(s): %w",
				JobTypeDocumentRasterize, len(rendered), len(req.Pages), ctxErr)), nil
		}

		img, renderErr := h.cfg.RenderPage(ctx, bin, req.PDF, page, req.DPI)
		if renderErr != nil {
			return h.failure(fmt.Errorf(
				"%s: rendering page %d failed: %w", JobTypeDocumentRasterize, page, renderErr)), nil
		}
		if len(img) == 0 {
			return h.failure(fmt.Errorf(
				"%s: rendering page %d produced no image", JobTypeDocumentRasterize, page)), nil
		}
		if len(img) > req.MaxBytesPerPage {
			return h.failure(fmt.Errorf(
				"%s: rendered page %d is %d bytes, over the %d byte per-page limit; ask for a lower dpi",
				JobTypeDocumentRasterize, page, len(img), req.MaxBytesPerPage)), nil
		}
		total += len(img)
		if total > rasterizeMaxTotalBytes {
			return h.failure(fmt.Errorf(
				"%s: rendered pages total more than the %d byte per-job limit; ask for fewer pages or a lower dpi",
				JobTypeDocumentRasterize, rasterizeMaxTotalBytes)), nil
		}

		rendered = append(rendered, map[string]any{
			"page":         page,
			"image_base64": base64.StdEncoding.EncodeToString(img),
			"mime":         rasterizeMimePNG,
			"bytes":        len(img),
		})
	}

	// The runner publishes this Output as the terminal `end` event, so `pages`
	// sits at the top level of what the caller validates.
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"pages":    rendered,
		"dpi":      req.DPI,
		"format":   rasterizeFormatPNG,
		"renderer": rasterizeRenderer,
	}}, nil
}

func (h *DocumentRasterizeHandler) failure(err error) *JobResult {
	return &JobResult{Status: JobStatusFailure, Error: err, Output: map[string]any{"error": err.Error()}}
}

// parseRasterizeRequest validates a document_rasterize payload into a request.
// Every rejection names what was wrong: the caller turns these into the message
// a human reads, so "invalid payload" would be useless.
func parseRasterizeRequest(payload map[string]any) (*RasterizeRequest, error) {
	if format := strings.ToLower(strings.TrimSpace(payloadString(payload, "format"))); format != "" &&
		format != rasterizeFormatPNG {
		return nil, fmt.Errorf("unsupported format %q, this node renders %q", format, rasterizeFormatPNG)
	}

	encoded := payloadString(payload, "pdf_base64")
	if encoded == "" {
		return nil, errors.New("no pdf_base64 in the payload")
	}
	pdf, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("pdf_base64 is not valid base64: %w", err)
	}
	if len(pdf) == 0 {
		return nil, errors.New("pdf_base64 decoded to no bytes")
	}
	if len(pdf) > rasterizeMaxPDFBytes {
		return nil, fmt.Errorf(
			"the document is %d bytes, over this node's %d byte render limit", len(pdf), rasterizeMaxPDFBytes)
	}
	// Cheap shape check before spawning a process. poppler would also refuse,
	// but its refusal reads as a syntax error rather than "this is not a PDF".
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return nil, errors.New("the decoded bytes are not a PDF (no %PDF- header)")
	}

	pages, err := rasterizePageNumbers(payload["pages"])
	if err != nil {
		return nil, err
	}

	dpi := payloadInt(payload, "dpi")
	if dpi == 0 {
		dpi = rasterizeDefaultDPI
	}
	if dpi < rasterizeMinDPI || dpi > rasterizeMaxDPI {
		return nil, fmt.Errorf(
			"dpi %d is outside the supported %d to %d range", dpi, rasterizeMinDPI, rasterizeMaxDPI)
	}

	maxPerPage := payloadInt(payload, "max_bytes_per_page")
	if maxPerPage <= 0 || maxPerPage > rasterizeMaxBytesPerPage {
		maxPerPage = rasterizeMaxBytesPerPage
	}

	return &RasterizeRequest{PDF: pdf, Pages: pages, DPI: dpi, MaxBytesPerPage: maxPerPage}, nil
}

// rasterizePageNumbers reads the 1-based page list. JSON decoding gives numbers
// as float64, so every numeric shape a payload can carry is coerced rather than
// type-asserted. The result is deduplicated and ascending, so a caller that
// repeats a page pays for one render.
func rasterizePageNumbers(raw any) ([]int, error) {
	list, ok := raw.([]any)
	if !ok {
		// A payload that went through a string-flattening transport arrives as
		// one value rather than a list; treat a single number as a single page.
		if single, isSingle := coerceInt(raw); isSingle {
			list = []any{single}
		} else {
			return nil, errors.New("no pages requested")
		}
	}
	if len(list) == 0 {
		return nil, errors.New("no pages requested")
	}
	if len(list) > rasterizeMaxPages {
		return nil, fmt.Errorf(
			"%d pages requested, over this node's %d page per-job limit", len(list), rasterizeMaxPages)
	}

	seen := make(map[int]struct{}, len(list))
	pages := make([]int, 0, len(list))
	for _, entry := range list {
		page, okNum := coerceInt(entry)
		if !okNum {
			return nil, fmt.Errorf("page number %v is not a number", entry)
		}
		if page < 1 {
			return nil, fmt.Errorf("page number %d is not 1-based", page)
		}
		if _, dup := seen[page]; dup {
			continue
		}
		seen[page] = struct{}{}
		pages = append(pages, page)
	}
	sort.Ints(pages)
	return pages, nil
}

// coerceInt reads an int out of any JSON-decoded numeric shape.
func coerceInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case string:
		var p int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &p); err == nil {
			return p, true
		}
	}
	return 0, false
}

// renderPageWithPdftoppm renders one page to PNG by piping the PDF through
// poppler. `-singlefile` with no output prefix makes pdftoppm write the image
// to stdout, so neither the document nor the rendered page touches the disk.
func renderPageWithPdftoppm(ctx context.Context, bin string, pdf []byte, page, dpi int) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin,
		"-png",
		"-r", fmt.Sprint(dpi),
		"-f", fmt.Sprint(page),
		"-l", fmt.Sprint(page),
		"-singlefile",
		"-", // read the PDF from stdin
	)
	cmd.Stdin = bytes.NewReader(pdf)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		// poppler exits 99 for a page outside the document, which is a caller
		// mistake worth naming rather than a generic render failure.
		return nil, errors.New(firstLine(detail))
	}
	if stdout.Len() == 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = "the renderer produced no output"
		}
		return nil, errors.New(firstLine(detail))
	}
	return stdout.Bytes(), nil
}

// firstLine keeps an error message to poppler's first diagnostic. It emits a
// warning cascade for a damaged file and all of it would drown the result.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

// Ensure DocumentRasterizeHandler implements JobHandler.
var _ JobHandler = (*DocumentRasterizeHandler)(nil)
