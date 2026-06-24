package desktop

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"io"
)

// RFB encoding numbers we care about.
const (
	encodingRaw  int32 = 0
	encodingZRLE int32 = 16
)

// zrleTileSize is the fixed tile dimension used by the ZRLE encoding (RFC 6143
// section 7.7.6). Tiles are 64x64, with partial tiles at the right/bottom edges.
const zrleTileSize = 64

// extractRect copies the pixels of the sub-rectangle (x, y, w, h) out of a
// full-frame capture into a contiguous, row-major RGBX buffer.
//
// The capture stores pixels in native BGRX byte order (GDI). This helper swaps
// B<->R as it copies so the returned buffer is in the RGBX order advertised in
// ServerInit. It is the single source of pixel bytes for both the Raw sub-rect
// path and the ZRLE tiler (which takes the first 3 bytes of each pixel as a
// compressed pixel / CPIXEL).
//
// The full frame is assumed to be frameW pixels wide and 4 bytes per pixel.
func extractRect(pix []byte, frameW, x, y, w, h int) []byte {
	out := make([]byte, w*h*4)
	for row := 0; row < h; row++ {
		srcStart := ((y+row)*frameW + x) * 4
		dstStart := row * w * 4
		src := pix[srcStart : srcStart+w*4]
		dst := out[dstStart : dstStart+w*4]
		// Copy with B<->R swap (BGRX -> RGBX).
		for i := 0; i < len(src); i += 4 {
			dst[i] = src[i+2]
			dst[i+1] = src[i+1]
			dst[i+2] = src[i]
			dst[i+3] = src[i+3]
		}
	}
	return out
}

// writeRawRectHeader writes the 12-byte rectangle header (x, y, w, h, encoding).
func writeRawRectHeader(buf *bytes.Buffer, x, y, w, h int, encoding int32) {
	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[0:2], uint16(x))
	binary.BigEndian.PutUint16(hdr[2:4], uint16(y))
	binary.BigEndian.PutUint16(hdr[4:6], uint16(w))
	binary.BigEndian.PutUint16(hdr[6:8], uint16(h))
	binary.BigEndian.PutUint32(hdr[8:12], uint32(encoding))
	buf.Write(hdr[:])
}

// encodeRawRect appends a Raw-encoded rectangle (header + RGBX pixel bytes) for
// the sub-rectangle (x, y, w, h) of the frame to buf.
func encodeRawRect(buf *bytes.Buffer, pix []byte, frameW, x, y, w, h int) {
	writeRawRectHeader(buf, x, y, w, h, encodingRaw)
	buf.Write(extractRect(pix, frameW, x, y, w, h))
}

// zrleEncoder holds the per-connection zlib stream that ZRLE rectangles are
// written to. RFB requires ONE continuous zlib stream for the lifetime of the
// connection (the dictionary carries across rects and frames), so this must be
// created once per client and reused, never per frame.
type zrleEncoder struct {
	zbuf bytes.Buffer
	zw   *zlib.Writer
}

func newZRLEEncoder() *zrleEncoder {
	e := &zrleEncoder{}
	e.zw = zlib.NewWriter(&e.zbuf)
	return e
}

// Close releases the underlying zlib writer.
func (e *zrleEncoder) Close() {
	if e.zw != nil {
		_ = e.zw.Close()
		e.zw = nil
	}
}

// encodeZRLERect appends a ZRLE-encoded rectangle for the sub-rectangle
// (x, y, w, h) of the frame to buf.
//
// The rectangle is split into 64x64 tiles (partial at edges). Each tile is
// encoded with subencoding 0 (raw CPIXELs): a single 0 byte followed by the
// tile's pixels in compressed-pixel form (3 bytes each, the unused 4th byte of
// the 32bpp RGBX format is dropped). The concatenated tile bytes are written to
// the connection's continuous zlib stream and flushed; the resulting deflated
// bytes form the rect payload, prefixed with a u32 length.
//
// Using only subencoding 0 keeps the implementation simple and robust: zlib
// provides the compression. Every standard VNC client decodes this.
func (e *zrleEncoder) encodeZRLERect(buf *bytes.Buffer, pix []byte, frameW, x, y, w, h int) error {
	writeRawRectHeader(buf, x, y, w, h, encodingZRLE)

	// Build the uncompressed ZRLE tile stream for this rect.
	tiles := buildZRLETiles(pix, frameW, x, y, w, h)

	// Write tiles to the continuous zlib stream and flush so the client can
	// decode this rect immediately. Flush emits a Z_SYNC_FLUSH boundary.
	e.zbuf.Reset()
	if _, err := e.zw.Write(tiles); err != nil {
		return err
	}
	if err := e.zw.Flush(); err != nil {
		return err
	}
	compressed := e.zbuf.Bytes()

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(compressed)))
	buf.Write(lenBuf[:])
	buf.Write(compressed)
	return nil
}

// buildZRLETiles produces the uncompressed ZRLE tile byte stream for the
// sub-rectangle (x, y, w, h). Each tile is subencoding 0 (raw) followed by its
// CPIXELs (3 bytes/pixel). Exported indirectly via tests.
func buildZRLETiles(pix []byte, frameW, x, y, w, h int) []byte {
	var out bytes.Buffer
	for ty := 0; ty < h; ty += zrleTileSize {
		th := zrleTileSize
		if ty+th > h {
			th = h - ty
		}
		for tx := 0; tx < w; tx += zrleTileSize {
			tw := zrleTileSize
			if tx+tw > w {
				tw = w - tx
			}
			// Subencoding 0 = raw CPIXELs.
			out.WriteByte(0)
			// CPIXELs: for each pixel in the tile, 3 bytes (R, G, B), dropping
			// the unused 4th byte of the RGBX format.
			for row := 0; row < th; row++ {
				srcY := y + ty + row
				rowStart := (srcY*frameW + x + tx) * 4
				for col := 0; col < tw; col++ {
					p := rowStart + col*4
					// Source is native BGRX; CPIXEL order must match the
					// advertised RGBX format, so emit R, G, B = pix[p+2],
					// pix[p+1], pix[p].
					out.WriteByte(pix[p+2])
					out.WriteByte(pix[p+1])
					out.WriteByte(pix[p])
				}
			}
		}
	}
	return out.Bytes()
}

// dirtyTiles compares the previous and current frames (both native BGRX,
// same dimensions) on a 64-aligned grid and returns the bounding rectangles of
// the changed tiles, coalesced row by row.
//
// prev may be nil (or a different size), in which case the whole frame is
// reported as a single dirty rectangle (a full update). This is the correct
// behaviour for the first frame of a connection and for non-incremental
// FramebufferUpdateRequests.
func dirtyTiles(prev, cur []byte, frameW, frameH int) []image.Rectangle {
	full := []image.Rectangle{image.Rect(0, 0, frameW, frameH)}
	if prev == nil || len(prev) != len(cur) {
		return full
	}

	var rects []image.Rectangle
	for ty := 0; ty < frameH; ty += zrleTileSize {
		th := zrleTileSize
		if ty+th > frameH {
			th = frameH - ty
		}
		// Track a run of contiguous changed tile-columns to coalesce into one
		// rectangle per band.
		runStart := -1
		for tx := 0; tx <= frameW; tx += zrleTileSize {
			changed := false
			if tx < frameW {
				tw := zrleTileSize
				if tx+tw > frameW {
					tw = frameW - tx
				}
				changed = tileChanged(prev, cur, frameW, tx, ty, tw, th)
			}
			if changed {
				if runStart < 0 {
					runStart = tx
				}
			} else if runStart >= 0 {
				rects = append(rects, image.Rect(runStart, ty, tx, ty+th))
				runStart = -1
			}
		}
	}
	return rects
}

// tileChanged reports whether any pixel in the tile (tx, ty, tw, th) differs
// between prev and cur.
func tileChanged(prev, cur []byte, frameW, tx, ty, tw, th int) bool {
	for row := 0; row < th; row++ {
		start := ((ty+row)*frameW + tx) * 4
		end := start + tw*4
		if !bytes.Equal(prev[start:end], cur[start:end]) {
			return true
		}
	}
	return false
}

// writeFramebufferUpdateRects writes a complete FramebufferUpdate message
// containing the given rectangles, encoded with the chosen encoding (ZRLE when
// zEnc is non-nil and the client negotiated it, else Raw). The pixel source is
// the native-BGRX frame; extraction handles the byte-order swap.
//
// rects must already be clamped to the frame bounds.
func writeFramebufferUpdateRects(w io.Writer, pix []byte, frameW int, rects []image.Rectangle, zEnc *zrleEncoder) error {
	var buf bytes.Buffer

	// Drop any degenerate rectangles up front so the number-of-rects header
	// always matches the rectangles actually written (a mismatch desyncs the
	// RFB stream).
	valid := rects[:0:0]
	for _, r := range rects {
		if r.Dx() > 0 && r.Dy() > 0 {
			valid = append(valid, r)
		}
	}

	// FramebufferUpdate header: type(0) + padding(1) + number-of-rects(u16).
	buf.WriteByte(0)
	buf.WriteByte(0)
	var nrects [2]byte
	binary.BigEndian.PutUint16(nrects[:], uint16(len(valid)))
	buf.Write(nrects[:])

	for _, r := range valid {
		x, y := r.Min.X, r.Min.Y
		rw, rh := r.Dx(), r.Dy()
		if zEnc != nil {
			if err := zEnc.encodeZRLERect(&buf, pix, frameW, x, y, rw, rh); err != nil {
				return err
			}
		} else {
			encodeRawRect(&buf, pix, frameW, x, y, rw, rh)
		}
	}

	_, err := w.Write(buf.Bytes())
	return err
}

// clientSupportsZRLE scans a SetEncodings list (big-endian s32 values) and
// reports whether the client advertised ZRLE (16). Pseudo-encodings (negative
// values) are ignored.
func clientSupportsZRLE(encodings []byte) bool {
	for i := 0; i+4 <= len(encodings); i += 4 {
		enc := int32(binary.BigEndian.Uint32(encodings[i : i+4]))
		if enc == encodingZRLE {
			return true
		}
	}
	return false
}
