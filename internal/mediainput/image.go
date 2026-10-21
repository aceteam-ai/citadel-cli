package mediainput

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/webp"
)

const MaxPixels = 16_777_216

func boundedDimensions(width, height int) error {
	if width < 1 || height < 1 || width > 65535 || height > 65535 || uint64(width)*uint64(height) > MaxPixels {
		return refuse("pixels_exceeded")
	}
	return nil
}

// headerDimensions never calls a full image decoder. PNG/JPEG use the standard
// library. WebP is handled separately so extended canvas and actual bitstream
// dimensions are both checked, not just the early-return VP8X header.
func headerDimensions(reader io.Reader, format string) (int, int, error) {
	var config image.Config
	var err error
	switch format {
	case "png":
		config, err = png.DecodeConfig(reader)
	case "jpeg":
		config, err = jpeg.DecodeConfig(reader)
	default:
		return 0, 0, refuse("invalid_format")
	}
	if err != nil {
		return 0, 0, refuse("invalid_image")
	}
	if err := boundedDimensions(config.Width, config.Height); err != nil {
		return 0, 0, err
	}
	return config.Width, config.Height, nil
}

func inspectImage(data []byte, format string, mask bool) (int, int, error) {
	if len(data) == 0 || len(data) > MaxFileBytes {
		return 0, 0, refuse("size_exceeded")
	}
	if mask && format != "png" {
		return 0, 0, refuse("mask_invalid")
	}
	if format == "webp" {
		return inspectWebP(data)
	}
	width, height, err := headerDimensions(bytes.NewReader(data), format)
	if err != nil {
		return 0, 0, err
	}
	switch format {
	case "png":
		if err := inspectPNGChunks(data, mask); err != nil {
			return 0, 0, err
		}
	case "jpeg":
		if err := inspectJPEGExif(data); err != nil {
			return 0, 0, err
		}
	}
	return width, height, nil
}

func inspectPNGChunks(data []byte, mask bool) error {
	if len(data) < 33 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return refuse("invalid_image")
	}
	if mask && (data[25] != 0 || (data[24] != 1 && data[24] != 8)) {
		return refuse("mask_invalid")
	}
	seenData := false
	for offset := 8; offset < len(data); {
		if len(data)-offset < 12 {
			return refuse("invalid_image")
		}
		size := uint64(binary.BigEndian.Uint32(data[offset:]))
		if size > uint64(len(data)-offset-12) {
			return refuse("invalid_image")
		}
		end := offset + 12 + int(size)
		kind := string(data[offset+4 : offset+8])
		if crc32.ChecksumIEEE(data[offset+4:end-4]) != binary.BigEndian.Uint32(data[end-4:end]) {
			return refuse("invalid_image")
		}
		switch kind {
		case "acTL", "fcTL", "fdAT":
			return refuse("animated_input")
		case "IHDR":
			if offset != 8 || size != 13 {
				return refuse("invalid_image")
			}
		case "eXIf":
			if err := checkEXIFOrientation(data[offset+8 : end-4]); err != nil {
				return err
			}
		case "IDAT":
			seenData = true
		case "IEND":
			if size != 0 || !seenData || end != len(data) {
				return refuse("invalid_image")
			}
			return nil
		}
		offset = end
	}
	return refuse("invalid_image")
}

func inspectJPEGExif(data []byte) error {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return refuse("invalid_image")
	}
	seenEXIF := false
	for offset := 2; offset < len(data); {
		if data[offset] != 0xff {
			return refuse("invalid_image")
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset == len(data) {
			return refuse("invalid_image")
		}
		marker := data[offset]
		offset++
		if marker == 0xda {
			// The header decoder already validated through Start Of Scan. Pixel
			// data is intentionally not decoded by this boundary.
			return nil
		}
		if marker == 0 || marker == 0xd8 || marker == 0xd9 {
			return refuse("invalid_image")
		}
		if marker == 1 || marker >= 0xd0 && marker <= 0xd7 {
			continue
		}
		if len(data)-offset < 2 {
			return refuse("invalid_image")
		}
		size := int(binary.BigEndian.Uint16(data[offset:]))
		if size < 2 || size > len(data)-offset {
			return refuse("invalid_image")
		}
		payload := data[offset+2 : offset+size]
		if marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			if seenEXIF {
				return refuse("invalid_image")
			}
			seenEXIF = true
			if err := checkEXIFOrientation(payload); err != nil {
				return err
			}
		}
		offset += size
	}
	return refuse("invalid_image")
}

// checkEXIFOrientation inspects only the bounded TIFF IFD0 orientation entry.
// It never applies a transform, follows external offsets or decodes pixel data.
func checkEXIFOrientation(data []byte) error {
	data = bytes.TrimPrefix(data, []byte("Exif\x00\x00"))
	if len(data) < 8 {
		return refuse("invalid_image")
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return refuse("invalid_image")
	}
	if order.Uint16(data[2:]) != 42 {
		return refuse("invalid_image")
	}
	offset := uint64(order.Uint32(data[4:]))
	if offset < 8 || offset+2 > uint64(len(data)) {
		return refuse("invalid_image")
	}
	count := uint64(order.Uint16(data[offset:]))
	if offset+2+12*count+4 > uint64(len(data)) {
		return refuse("invalid_image")
	}
	seen := false
	for i := uint64(0); i < count; i++ {
		entry := data[offset+2+12*i:]
		if order.Uint16(entry) != 0x0112 {
			continue
		}
		if seen || order.Uint16(entry[2:]) != 3 || order.Uint32(entry[4:]) != 1 {
			return refuse("invalid_image")
		}
		seen = true
		if order.Uint16(entry[8:]) != 1 {
			return refuse("exif_orientation_unsupported")
		}
	}
	return nil
}

func inspectWebP(data []byte) (int, int, error) {
	invalid := refuse("invalid_image")
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return 0, 0, invalid
	}
	var canvasW, canvasH, width, height int
	seenImage, seenExtended, seenEXIF := false, false, false
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return 0, 0, invalid
		}
		size := uint64(binary.LittleEndian.Uint32(data[offset+4:]))
		padded := size + size%2
		if padded > uint64(len(data)-offset-8) {
			return 0, 0, invalid
		}
		payload := data[offset+8 : offset+8+int(size)]
		if size%2 != 0 && data[offset+8+int(size)] != 0 {
			return 0, 0, invalid
		}
		kind := string(data[offset : offset+4])
		switch kind {
		case "ANIM", "ANMF":
			return 0, 0, refuse("animated_input")
		case "VP8X":
			if offset != 12 || seenExtended || len(payload) != 10 || payload[0]&0xc1 != 0 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
				return 0, 0, invalid
			}
			if payload[0]&2 != 0 {
				return 0, 0, refuse("animated_input")
			}
			seenExtended = true
			canvasW = 1 + int(payload[4]) + int(payload[5])<<8 + int(payload[6])<<16
			canvasH = 1 + int(payload[7]) + int(payload[8])<<8 + int(payload[9])<<16
			if err := boundedDimensions(canvasW, canvasH); err != nil {
				return 0, 0, err
			}
		case "VP8 ", "VP8L":
			if seenImage || (!seenExtended && offset != 12) {
				return 0, 0, invalid
			}
			// Present only this image chunk to DecodeConfig, so a VP8X early
			// return cannot hide different underlying bitstream dimensions.
			var header [20]byte
			copy(header[:4], "RIFF")
			binary.LittleEndian.PutUint32(header[4:8], uint32(12+padded))
			copy(header[8:12], "WEBP")
			copy(header[12:16], kind)
			binary.LittleEndian.PutUint32(header[16:20], uint32(size))
			config, err := webp.DecodeConfig(io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(payload)))
			if err != nil {
				return 0, 0, invalid
			}
			width, height = config.Width, config.Height
			if err := boundedDimensions(width, height); err != nil {
				return 0, 0, err
			}
			seenImage = true
		case "EXIF":
			if !seenExtended || seenEXIF {
				return 0, 0, invalid
			}
			seenEXIF = true
			if err := checkEXIFOrientation(payload); err != nil {
				return 0, 0, err
			}
		default:
			if !seenExtended {
				return 0, 0, invalid
			}
		}
		offset += 8 + int(padded)
	}
	if !seenImage || seenExtended && (width != canvasW || height != canvasH) {
		return 0, 0, invalid
	}
	return width, height, nil
}
