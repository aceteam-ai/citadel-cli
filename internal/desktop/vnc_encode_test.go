package desktop

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"io"
	"testing"
)

// makeBGRXFrame builds a w*h frame in native BGRX byte order where each pixel's
// R,G,B are derived from its position, so byte-order handling is observable.
func makeBGRXFrame(w, h int) []byte {
	pix := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p := (y*w + x) * 4
			r := byte((x * 7) & 0xff)
			g := byte((y * 11) & 0xff)
			b := byte((x + y) & 0xff)
			// Native BGRX order.
			pix[p] = b
			pix[p+1] = g
			pix[p+2] = r
			pix[p+3] = 0
		}
	}
	return pix
}

func TestExtractRectSwapsToRGBX(t *testing.T) {
	w, h := 4, 3
	pix := makeBGRXFrame(w, h)
	// Extract a 2x2 sub-rect at (1,1).
	out := extractRect(pix, w, 1, 1, 2, 2)
	if len(out) != 2*2*4 {
		t.Fatalf("len = %d, want 16", len(out))
	}
	// Verify the top-left pixel of the sub-rect (source pixel (1,1)).
	srcP := (1*w + 1) * 4
	wantR, wantG, wantB := pix[srcP+2], pix[srcP+1], pix[srcP]
	if out[0] != wantR || out[1] != wantG || out[2] != wantB {
		t.Errorf("pixel = (%d,%d,%d), want RGB (%d,%d,%d)", out[0], out[1], out[2], wantR, wantG, wantB)
	}
}

func TestEncodeRawRectHeaderAndPixels(t *testing.T) {
	w, h := 8, 8
	pix := makeBGRXFrame(w, h)
	var buf bytes.Buffer
	encodeRawRect(&buf, pix, w, 2, 1, 3, 4)

	data := buf.Bytes()
	// 12-byte rect header.
	if x := binary.BigEndian.Uint16(data[0:2]); x != 2 {
		t.Errorf("x = %d, want 2", x)
	}
	if y := binary.BigEndian.Uint16(data[2:4]); y != 1 {
		t.Errorf("y = %d, want 1", y)
	}
	if rw := binary.BigEndian.Uint16(data[4:6]); rw != 3 {
		t.Errorf("w = %d, want 3", rw)
	}
	if rh := binary.BigEndian.Uint16(data[6:8]); rh != 4 {
		t.Errorf("h = %d, want 4", rh)
	}
	if enc := binary.BigEndian.Uint32(data[8:12]); enc != 0 {
		t.Errorf("encoding = %d, want 0 (Raw)", enc)
	}
	// Payload length = w*h*4.
	if got, want := len(data)-12, 3*4*4; got != want {
		t.Errorf("payload len = %d, want %d", got, want)
	}
}

func TestBuildZRLETilesSingleTileCPIXEL(t *testing.T) {
	// 2x2 frame -> one tile, 4 pixels, subencoding byte + 4*3 CPIXEL bytes.
	w, h := 2, 2
	pix := makeBGRXFrame(w, h)
	tiles := buildZRLETiles(pix, w, 0, 0, w, h)

	wantLen := 1 + (w*h)*3
	if len(tiles) != wantLen {
		t.Fatalf("tiles len = %d, want %d", len(tiles), wantLen)
	}
	if tiles[0] != 0 {
		t.Errorf("subencoding = %d, want 0 (raw)", tiles[0])
	}
	// First CPIXEL should be the top-left pixel in R,G,B order (3 bytes, no 4th).
	srcP := 0
	wantR, wantG, wantB := pix[srcP+2], pix[srcP+1], pix[srcP]
	if tiles[1] != wantR || tiles[2] != wantG || tiles[3] != wantB {
		t.Errorf("CPIXEL = (%d,%d,%d), want (%d,%d,%d)", tiles[1], tiles[2], tiles[3], wantR, wantG, wantB)
	}
}

func TestBuildZRLETilesPartialEdgeTiles(t *testing.T) {
	// 65x65 frame -> 2x2 tile grid: 64,64 | 64,1 | 1,64 | 1,1 (partial edges).
	w, h := zrleTileSize+1, zrleTileSize+1
	pix := makeBGRXFrame(w, h)
	tiles := buildZRLETiles(pix, w, 0, 0, w, h)

	// Four tiles: pixel counts 64*64, 1*64, 64*1, 1*1 = 4096+64+64+1 = 4225 px.
	// Plus 4 subencoding bytes. Each pixel = 3 CPIXEL bytes.
	totalPx := w * h // 65*65 = 4225 = sum above
	wantLen := 4 /*subenc bytes*/ + totalPx*3
	if len(tiles) != wantLen {
		t.Fatalf("tiles len = %d, want %d", len(tiles), wantLen)
	}
}

// TestEncodeZRLERectRoundTrip encodes a rect, then decompresses the zlib stream
// and verifies the tile bytes match what buildZRLETiles produced. This is a
// byte-level check that needs no display.
func TestEncodeZRLERectRoundTrip(t *testing.T) {
	w, h := 70, 50 // spans 2 tile columns, 1 tile row (partial)
	pix := makeBGRXFrame(w, h)

	enc := newZRLEEncoder()
	defer enc.Close()

	var buf bytes.Buffer
	if err := enc.encodeZRLERect(&buf, pix, w, 0, 0, w, h); err != nil {
		t.Fatalf("encodeZRLERect: %v", err)
	}

	data := buf.Bytes()
	// 12-byte rect header.
	if enc := binary.BigEndian.Uint32(data[8:12]); int32(enc) != encodingZRLE {
		t.Errorf("encoding = %d, want %d (ZRLE)", enc, encodingZRLE)
	}
	// u32 length prefix.
	zlen := binary.BigEndian.Uint32(data[12:16])
	if int(zlen) != len(data)-16 {
		t.Fatalf("zlib length prefix = %d, but %d bytes follow", zlen, len(data)-16)
	}

	// Decompress and compare against the expected tile stream.
	zr, err := zlib.NewReader(bytes.NewReader(data[16:]))
	if err != nil {
		t.Fatalf("zlib.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil && err != io.ErrUnexpectedEOF {
		// A flushed (not closed) zlib stream may surface ErrUnexpectedEOF at the
		// flush boundary; the decoded payload before it is still valid.
		t.Fatalf("read zlib: %v", err)
	}
	want := buildZRLETiles(pix, w, 0, 0, w, h)
	if !bytes.Equal(got, want) {
		t.Errorf("decoded tiles mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
}

// TestZRLEContinuousStream verifies that two rects encoded on the same encoder
// share one continuous zlib stream (the second is decoded by continuing the
// first reader), which is what RFB requires.
func TestZRLEContinuousStream(t *testing.T) {
	w, h := 16, 16
	pix := makeBGRXFrame(w, h)

	enc := newZRLEEncoder()
	defer enc.Close()

	var buf1, buf2 bytes.Buffer
	if err := enc.encodeZRLERect(&buf1, pix, w, 0, 0, w, h); err != nil {
		t.Fatalf("rect1: %v", err)
	}
	if err := enc.encodeZRLERect(&buf2, pix, w, 0, 0, w, h); err != nil {
		t.Fatalf("rect2: %v", err)
	}

	// Concatenate both rects' zlib payloads and decode as a single stream.
	payload1 := buf1.Bytes()[16:]
	payload2 := buf2.Bytes()[16:]
	combined := append(append([]byte{}, payload1...), payload2...)

	zr, err := zlib.NewReader(bytes.NewReader(combined))
	if err != nil {
		t.Fatalf("zlib.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil && err != io.ErrUnexpectedEOF {
		t.Fatalf("read: %v", err)
	}
	tile := buildZRLETiles(pix, w, 0, 0, w, h)
	want := append(append([]byte{}, tile...), tile...)
	if !bytes.Equal(got, want) {
		t.Errorf("continuous stream decode mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func TestClientSupportsZRLE(t *testing.T) {
	// Build a SetEncodings list: [Tight(7), ZRLE(16), Cursor pseudo(-239)].
	list := make([]byte, 0, 12)
	for _, e := range []int32{7, encodingZRLE, -239} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(e))
		list = append(list, b[:]...)
	}
	if !clientSupportsZRLE(list) {
		t.Error("expected ZRLE to be detected")
	}

	// List without ZRLE.
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], 0)
	if clientSupportsZRLE(raw[:]) {
		t.Error("did not expect ZRLE in a Raw-only list")
	}

	// Truncated trailing bytes must not panic or false-positive.
	if clientSupportsZRLE([]byte{0, 0, 0}) {
		t.Error("truncated list should not report ZRLE")
	}
}

func TestDirtyTilesNilPrevReturnsFull(t *testing.T) {
	cur := makeBGRXFrame(100, 100)
	rects := dirtyTiles(nil, cur, 100, 100)
	if len(rects) != 1 || rects[0] != image.Rect(0, 0, 100, 100) {
		t.Errorf("nil prev should yield one full rect, got %v", rects)
	}
}

func TestDirtyTilesSizeMismatchReturnsFull(t *testing.T) {
	prev := makeBGRXFrame(50, 50)
	cur := makeBGRXFrame(100, 100)
	rects := dirtyTiles(prev, cur, 100, 100)
	if len(rects) != 1 || rects[0] != image.Rect(0, 0, 100, 100) {
		t.Errorf("size mismatch should yield one full rect, got %v", rects)
	}
}

func TestDirtyTilesNoChange(t *testing.T) {
	frame := makeBGRXFrame(130, 130)
	prev := append([]byte{}, frame...)
	rects := dirtyTiles(prev, frame, 130, 130)
	if len(rects) != 0 {
		t.Errorf("identical frames should yield no dirty rects, got %v", rects)
	}
}

func TestDirtyTilesSingleTileChange(t *testing.T) {
	w, h := 200, 200 // 4x4 tile grid (last tiles partial: 200 = 3*64 + 8)
	frame := makeBGRXFrame(w, h)
	prev := append([]byte{}, frame...)

	// Mutate one pixel inside the tile at column index 1 (x in [64,128)),
	// row index 2 (y in [128,192)).
	px, py := 70, 130
	frame[(py*w+px)*4] ^= 0xff

	rects := dirtyTiles(prev, frame, w, h)
	if len(rects) != 1 {
		t.Fatalf("expected 1 dirty rect, got %d: %v", len(rects), rects)
	}
	r := rects[0]
	// The dirty rect must contain the mutated pixel.
	if !(image.Point{X: px, Y: py}).In(r) {
		t.Errorf("dirty rect %v does not contain mutated pixel (%d,%d)", r, px, py)
	}
	// And it must be within a single tile band (height 64 starting at y=128).
	if r.Min.Y != 128 || r.Max.Y != 192 {
		t.Errorf("dirty rect y-band = [%d,%d), want [128,192)", r.Min.Y, r.Max.Y)
	}
}

func TestDirtyTilesCoalescesRowRun(t *testing.T) {
	w, h := 200, 70 // tile rows: [0,64),[64,70); cols at 0,64,128,192
	frame := makeBGRXFrame(w, h)
	prev := append([]byte{}, frame...)

	// Change two adjacent tile columns in the top band: x=10 (col0) and x=70 (col1).
	frame[(5*w+10)*4] ^= 0xff
	frame[(5*w+70)*4] ^= 0xff

	rects := dirtyTiles(prev, frame, w, h)
	if len(rects) != 1 {
		t.Fatalf("expected adjacent changed columns to coalesce into 1 rect, got %d: %v", len(rects), rects)
	}
	r := rects[0]
	if r.Min.X != 0 || r.Max.X != 128 {
		t.Errorf("coalesced rect x-span = [%d,%d), want [0,128)", r.Min.X, r.Max.X)
	}
}

func TestWriteFramebufferUpdateRectsRawNrects(t *testing.T) {
	w, h := 64, 64
	pix := makeBGRXFrame(w, h)
	rects := []image.Rectangle{
		image.Rect(0, 0, 32, 32),
		image.Rect(32, 0, 64, 32),
	}
	var buf bytes.Buffer
	if err := writeFramebufferUpdateRects(&buf, pix, w, rects, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	data := buf.Bytes()
	if data[0] != 0 {
		t.Errorf("message type = %d, want 0", data[0])
	}
	if n := binary.BigEndian.Uint16(data[2:4]); n != 2 {
		t.Errorf("nrects = %d, want 2", n)
	}
}
