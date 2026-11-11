package mediainput

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"testing"
)

func encodedPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func encodedJPEG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := jpeg.Encode(&output, image.NewGray(image.Rect(0, 0, 2, 3)), nil); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func pngChunk(kind string, payload []byte) []byte {
	result := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(result, uint32(len(payload)))
	copy(result[4:8], kind)
	copy(result[8:], payload)
	binary.BigEndian.PutUint32(result[len(result)-4:], crc32.ChecksumIEEE(result[4:len(result)-4]))
	return result
}

func exifOrientation(order binary.ByteOrder, value uint16) []byte {
	data := make([]byte, 26)
	copy(data, "II")
	if order == binary.BigEndian {
		copy(data, "MM")
	}
	order.PutUint16(data[2:], 42)
	order.PutUint32(data[4:], 8)
	order.PutUint16(data[8:], 1)
	order.PutUint16(data[10:], 0x0112)
	order.PutUint16(data[12:], 3)
	order.PutUint32(data[14:], 1)
	order.PutUint16(data[18:], value)
	return data
}

func jpegWithExif(original, exif []byte) []byte {
	payload := append([]byte("Exif\x00\x00"), exif...)
	header := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(header[2:], uint16(len(payload)+2))
	result := append([]byte(nil), original[:2]...)
	result = append(result, header...)
	result = append(result, payload...)
	return append(result, original[2:]...)
}

func riffChunk(kind string, payload []byte) []byte {
	result := make([]byte, 8+len(payload)+len(payload)%2)
	copy(result, kind)
	binary.LittleEndian.PutUint32(result[4:], uint32(len(payload)))
	copy(result[8:], payload)
	return result
}

func webpContainer(chunks ...[]byte) []byte {
	result := []byte("RIFF\x00\x00\x00\x00WEBP")
	for _, chunk := range chunks {
		result = append(result, chunk...)
	}
	binary.LittleEndian.PutUint32(result[4:], uint32(len(result)-8))
	return result
}

// Header fixtures deliberately stop short of encoded pixel data. These test
// header bounds, not full image decode or real-model compatibility.
func webpLosslessHeader(width, height int) []byte {
	data := make([]byte, 5)
	data[0] = 0x2f
	binary.LittleEndian.PutUint32(data[1:], uint32(width-1)|uint32(height-1)<<14)
	return riffChunk("VP8L", data)
}

func webpLossyHeader(width, height int) []byte {
	data := []byte{0x10, 0, 0, 0x9d, 1, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(data[6:], uint16(width))
	binary.LittleEndian.PutUint16(data[8:], uint16(height))
	return riffChunk("VP8 ", data)
}

func webpExtendedHeader(width, height int, flags byte) []byte {
	data := make([]byte, 10)
	data[0] = flags
	for i := 0; i < 3; i++ {
		data[4+i] = byte((width - 1) >> (8 * i))
		data[7+i] = byte((height - 1) >> (8 * i))
	}
	return riffChunk("VP8X", data)
}

func assertReason(t *testing.T, err error, want string) {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Reason() != want {
		t.Fatalf("error=%v, want %s", err, want)
	}
}

func TestImageHeaderFormats(t *testing.T) {
	for _, tc := range []struct {
		name, format  string
		data          []byte
		width, height int
	}{
		{"png", "png", encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3))), 2, 3},
		{"jpeg", "jpeg", encodedJPEG(t), 2, 3},
		{"webp-lossy", "webp", webpContainer(webpLossyHeader(2, 3)), 2, 3},
		{"webp-lossless", "webp", webpContainer(webpLosslessHeader(2, 3)), 2, 3},
		{"webp-extended", "webp", webpContainer(webpExtendedHeader(257, 511, 0), webpLosslessHeader(257, 511)), 257, 511},
	} {
		t.Run(tc.name, func(t *testing.T) {
			width, height, err := inspectImage(tc.data, tc.format, false)
			if err != nil || width != tc.width || height != tc.height {
				t.Fatalf("dimensions=%dx%d, want %dx%d: %v", width, height, tc.width, tc.height, err)
			}
		})
	}
}

type countingReader struct {
	reader io.Reader
	read   int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestPNGBombStopsAtHeader(t *testing.T) {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, 8193)
	binary.BigEndian.PutUint32(ihdr[4:], 8193)
	ihdr[8] = 8
	reader := &countingReader{reader: bytes.NewReader(append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...))}
	_, _, err := headerDimensions(reader, "png")
	assertReason(t, err, "pixels_exceeded")
	if reader.read > 33 {
		t.Fatalf("bomb read beyond IHDR: %d bytes", reader.read)
	}
	if err := boundedDimensions(4096, 4096); err != nil {
		t.Fatal("exact pixel bound refused")
	}
	for _, size := range [][2]int{{0, 1}, {65536, 1}, {4096, 4097}} {
		assertReason(t, boundedDimensions(size[0], size[1]), "pixels_exceeded")
	}
}

func TestPNGAnimationAndMaskModes(t *testing.T) {
	gray := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	if _, _, err := inspectImage(gray, "png", true); err != nil {
		t.Fatal(err)
	}
	animated := append(append([]byte(nil), gray[:33]...), pngChunk("acTL", make([]byte, 8))...)
	animated = append(animated, gray[33:]...)
	_, _, err := inspectImage(animated, "png", false)
	assertReason(t, err, "animated_input")
	for _, img := range []image.Image{
		image.NewRGBA(image.Rect(0, 0, 2, 3)),
		image.NewGray16(image.Rect(0, 0, 2, 3)),
		image.NewPaletted(image.Rect(0, 0, 2, 3), color.Palette{color.Black, color.White}),
	} {
		_, _, err := inspectImage(encodedPNG(t, img), "png", true)
		assertReason(t, err, "mask_invalid")
	}
	// A real one-bit grayscale PNG with one filter byte and one packed row.
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, 1)
	binary.BigEndian.PutUint32(ihdr[4:], 1)
	ihdr[8] = 1
	oneBit := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...)
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	oneBit = append(oneBit, pngChunk("IDAT", compressed.Bytes())...)
	oneBit = append(oneBit, pngChunk("IEND", nil)...)
	if _, err := png.Decode(bytes.NewReader(oneBit)); err != nil {
		t.Fatalf("invalid one-bit fixture: %v", err)
	}
	if _, _, err := inspectImage(oneBit, "png", true); err != nil {
		t.Fatal(err)
	}
}

func TestEXIFRefusesTransformsAndMalformedOffsets(t *testing.T) {
	jpegData := encodedJPEG(t)
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for orientation := uint16(1); orientation <= 8; orientation++ {
			exif := exifOrientation(order, orientation)
			for _, tc := range []struct {
				format string
				data   []byte
			}{
				{"jpeg", jpegWithExif(jpegData, exif)},
				{"webp", webpContainer(webpExtendedHeader(2, 3, 8), webpLosslessHeader(2, 3), riffChunk("EXIF", exif))},
			} {
				_, _, err := inspectImage(tc.data, tc.format, false)
				if orientation == 1 {
					if err != nil {
						t.Fatalf("orientation=1 refused for %s: %v", tc.format, err)
					}
				} else {
					assertReason(t, err, "exif_orientation_unsupported")
				}
			}
		}
	}
	bad := exifOrientation(binary.LittleEndian, 1)
	binary.LittleEndian.PutUint32(bad[4:], 0xffffffff)
	assertReason(t, checkEXIFOrientation(bad), "invalid_image")
	for _, bad := range [][]byte{nil, []byte("Exif\x00\x00II"), make([]byte, 26)} {
		assertReason(t, checkEXIFOrientation(bad), "invalid_image")
	}
}

func TestWebPContainerRefusals(t *testing.T) {
	for _, data := range [][]byte{
		webpContainer(webpExtendedHeader(2, 3, 2), webpLosslessHeader(2, 3)),
		webpContainer(webpExtendedHeader(2, 3, 0), webpLosslessHeader(2, 3), riffChunk("ANIM", nil)),
		webpContainer(webpExtendedHeader(2, 3, 0), riffChunk("ANMF", nil)),
	} {
		_, _, err := inspectImage(data, "webp", false)
		assertReason(t, err, "animated_input")
	}
	for _, data := range [][]byte{
		webpContainer(webpExtendedHeader(2, 3, 0)),
		webpContainer(webpExtendedHeader(2, 3, 0), webpLosslessHeader(3, 3)),
		webpContainer(webpLosslessHeader(2, 3), webpLosslessHeader(2, 3)),
		webpContainer(riffChunk("VP8 ", []byte{0})),
		webpContainer(riffChunk("VP8L", []byte{0})),
		webpContainer(riffChunk("VP8X", []byte{0})),
		append(webpContainer(webpLosslessHeader(2, 3)), 0),
	} {
		_, _, err := inspectImage(data, "webp", false)
		assertReason(t, err, "invalid_image")
	}
	_, _, err := inspectImage(webpContainer(webpExtendedHeader(8193, 8193, 0), webpLosslessHeader(2, 3)), "webp", false)
	assertReason(t, err, "pixels_exceeded")
}

func TestImageTruncationsAndWrongFormats(t *testing.T) {
	for _, tc := range []struct {
		format string
		data   []byte
	}{
		{"png", encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))},
		{"jpeg", encodedJPEG(t)},
		{"webp", webpContainer(webpLosslessHeader(2, 3))},
	} {
		for length := 0; length < len(tc.data); length++ {
			// JPEG headers are deliberately sufficient once Start Of Scan is
			// present. Compressed payload validation belongs to the sidecar.
			if tc.format == "jpeg" && bytes.Contains(tc.data[:length], []byte{0xff, 0xda}) {
				break
			}
			if _, _, err := inspectImage(tc.data[:length], tc.format, false); err == nil {
				t.Fatalf("%s truncation %d accepted", tc.format, length)
			}
		}
		other := "png"
		if tc.format == "png" {
			other = "jpeg"
		}
		if _, _, err := inspectImage(tc.data, other, false); err == nil {
			t.Fatal("declared format mismatch accepted")
		}
	}
}

func FuzzInspectImage(f *testing.F) {
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		f.Fatal(err)
	}
	f.Add("png", pngData.Bytes(), false)
	f.Add("png", pngData.Bytes(), true)
	f.Add("webp", webpContainer(webpLosslessHeader(2, 3)), false)
	f.Add("jpeg", []byte{0xff, 0xd8, 0xff, 0xd9}, false)
	f.Fuzz(func(t *testing.T, format string, data []byte, mask bool) {
		width, height, err := inspectImage(data, format, mask)
		if err != nil {
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatal("image parser returned an unsanitized error")
			}
			return
		}
		if boundedDimensions(width, height) != nil || len(data) > MaxFileBytes || mask && format != "png" {
			t.Fatal("accepted image violates bounds")
		}
	})
}
