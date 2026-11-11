package cmd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	videoComposeMP4MetadataBoxBytes = int64(1 << 20)
	videoComposeMP4MetadataBytes    = int64(4 << 20)
)

type videoComposeMP4Box struct {
	kind                string
	start, payload, end int64
	header              int64
}

type videoComposeMP4Walker struct {
	reader        io.ReaderAt
	boxes         int
	moovs         int
	mdats         int
	tracks        int
	drefs         int
	metadataBytes int64
	haveMDTA      bool
}

type videoComposeMP4Track struct {
	handler  string
	haveSTSD bool
}

// This bounded preflight is not a codec parser or arbitrary camera guarantee.
// Metadata uses its own closed role grammar because FFmpeg's MOV dispatcher
// recursively interprets recognized atoms even inside meta/udta/ilst. Forced
// MOV demuxing and disabled external references remain mandatory afterward.
func validateVideoComposeMP4(path string) (retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 32 {
		return fmt.Errorf("not a self-contained MP4")
	}
	w := videoComposeMP4Walker{reader: f}
	if err := w.movie(0, info.Size(), "", 0, nil); err != nil {
		return err
	}
	if w.moovs != 1 || w.mdats == 0 || w.tracks < 1 || w.tracks > 2 {
		return fmt.Errorf("MP4 requires local movie, tracks and media data")
	}
	return nil
}

func (w *videoComposeMP4Walker) box(offset, end int64) (videoComposeMP4Box, error) {
	w.boxes++
	if w.boxes > 4096 || offset < 0 || end-offset < 8 {
		return videoComposeMP4Box{}, fmt.Errorf("invalid or excessive MP4 boxes")
	}
	var header [16]byte
	if _, err := w.reader.ReadAt(header[:8], offset); err != nil {
		return videoComposeMP4Box{}, err
	}
	size, headerSize := int64(binary.BigEndian.Uint32(header[:4])), int64(8)
	if size == 1 {
		if end-offset < 16 {
			return videoComposeMP4Box{}, fmt.Errorf("truncated MP4 extended box")
		}
		if _, err := w.reader.ReadAt(header[8:], offset+8); err != nil {
			return videoComposeMP4Box{}, err
		}
		extended := binary.BigEndian.Uint64(header[8:])
		if extended > uint64(end-offset) {
			return videoComposeMP4Box{}, fmt.Errorf("MP4 box exceeds parent")
		}
		size, headerSize = int64(extended), 16
	} else if size == 0 {
		size = end - offset
	}
	if size < headerSize || size > end-offset {
		return videoComposeMP4Box{}, fmt.Errorf("invalid MP4 box size")
	}
	return videoComposeMP4Box{kind: string(header[4:8]), start: offset, payload: offset + headerSize, end: offset + size, header: headerSize}, nil
}

func (w *videoComposeMP4Walker) metadataBox(box videoComposeMP4Box, root bool) error {
	if !root && box.kind == "uuid" {
		return fmt.Errorf("unsupported_uuid_box")
	}
	size := box.end - box.start
	if size > videoComposeMP4MetadataBoxBytes {
		return fmt.Errorf("metadata_box_too_large")
	}
	// Charge disjoint outer metadata regions, not the same serialized bytes
	// once for every nesting level. Every child still gets its individual cap.
	if root {
		if size > videoComposeMP4MetadataBytes-w.metadataBytes {
			return fmt.Errorf("metadata_budget_exceeded")
		}
		w.metadataBytes += size
	}
	return nil
}

func (w *videoComposeMP4Walker) movie(start, end int64, parent string, depth int, track *videoComposeMP4Track) error {
	if depth > 6 {
		return fmt.Errorf("MP4 nesting exceeds limit")
	}
	for offset := start; offset < end; {
		box, err := w.box(offset, end)
		if err != nil {
			return err
		}
		// Chapter references can change a track's role after sample parsing.
		// Refuse the dispatched leaf at every movie/sample/wave depth.
		if box.kind == "chap" {
			return fmt.Errorf("unsupported_chapter_reference")
		}
		if parent == "" {
			if offset == 0 && box.kind != "ftyp" {
				return fmt.Errorf("chapter must start with MP4 ftyp, not a manifest")
			}
			switch box.kind {
			case "ftyp":
				if offset != 0 || box.end-box.payload < 8 || box.end-box.payload > 1024 || (box.end-box.payload)%4 != 0 {
					return fmt.Errorf("invalid MP4 ftyp")
				}
				var brand [4]byte
				if _, err := w.reader.ReadAt(brand[:], box.payload); err != nil {
					return err
				}
				switch string(brand[:]) {
				case "isom", "iso2", "iso3", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "qt  ":
				default:
					return fmt.Errorf("unsupported MP4 brand")
				}
			case "moov":
				w.moovs++
				if w.moovs != 1 || box.end-box.start > 16<<20 {
					return fmt.Errorf("invalid or excessive MP4 moov")
				}
			case "mdat":
				w.mdats++
			case "free", "skip", "wide", "meta", "udta", "uuid":
			default:
				return fmt.Errorf("unsupported MP4 top-level box %q", box.kind)
			}
		}
		switch box.kind {
		case "moov", "trak", "mdia", "minf", "dinf", "stbl", "edts", "tref":
			// FFmpeg n6.1 dispatches recognized atoms inside all these
			// containers, including sample tables, edit lists and references.
			// They cannot be treated as opaque merely because metadata is not
			// normally placed there.
			parents := map[string]string{"moov": "", "trak": "moov", "mdia": "trak", "minf": "mdia", "dinf": "minf", "stbl": "minf", "edts": "trak", "tref": "trak"}
			if parent != parents[box.kind] {
				return fmt.Errorf("unexpected MP4 container %q", box.kind)
			}
			before := w.drefs
			childTrack := track
			if box.kind == "trak" {
				childTrack = &videoComposeMP4Track{}
			}
			if err := w.movie(box.payload, box.end, box.kind, depth+1, childTrack); err != nil {
				return err
			}
			if box.kind == "trak" {
				w.tracks++
				if w.drefs-before != 1 {
					return fmt.Errorf("MP4 track requires one self-contained data reference")
				}
			}
		case "hdlr":
			if err := w.mediaHandler(box, parent, track); err != nil {
				return err
			}
		case "stsd":
			if parent != "stbl" || track == nil || track.handler == "" || track.haveSTSD {
				return fmt.Errorf("unsupported_sample_description")
			}
			track.haveSTSD = true
			if err := w.sampleDescriptions(box, depth+1, track); err != nil {
				return err
			}
		case "wave":
			if parent != "sample-entry" || track == nil || track.handler != "soun" {
				return fmt.Errorf("unsupported MP4 container %q", box.kind)
			}
			if err := w.wave(box, depth, track); err != nil {
				return err
			}
		case "dref":
			if parent != "dinf" || box.end-box.payload != 20 {
				return fmt.Errorf("external or unsupported MP4 data reference")
			}
			var data [20]byte
			if _, err := w.reader.ReadAt(data[:], box.payload); err != nil {
				return err
			}
			if binary.BigEndian.Uint32(data[:4]) != 0 || binary.BigEndian.Uint32(data[4:8]) != 1 || binary.BigEndian.Uint32(data[8:12]) != 12 || string(data[12:16]) != "url " || binary.BigEndian.Uint32(data[16:20]) != 1 {
				return fmt.Errorf("MP4 data reference must be self-contained")
			}
			w.drefs++
		case "cmov", "rmra", "rmda", "rdrf":
			return fmt.Errorf("compressed/reference movie containers are unsupported")
		case "moof", "mvex", "traf", "sinf", "schi", "hoov":
			// Fragment/encryption containers and the legacy hoov alias are
			// also dispatched by MOV. This self-contained, non-fragmented
			// grammar does not support them at any depth.
			return fmt.Errorf("unsupported MP4 container %q", box.kind)
		case "wfex":
			// ff_get_wav_header changes the current stream to audio even
			// outside an audio sample entry. That can change a later stsd
			// header/extension boundary independently of its mdia handler.
			// This alternate WAVEFORMATEX form is not in the grammar.
			return fmt.Errorf("unsupported_sample_description")
		case "ftyp", "mdat", "wide":
			if parent != "" {
				return fmt.Errorf("unexpected MP4 container %q", box.kind)
			}
		case "url ", "urn ":
			// The only accepted reference entry is the exact self-contained
			// url checked inside dref above, never a separately dispatched atom.
			return fmt.Errorf("external or unsupported MP4 data reference")
		case "ilst", "keys":
			// These are accepted only by the closed metadata-role walker.
			return fmt.Errorf("unsupported_metadata_box")
		case "meta", "udta":
			if parent != "" && parent != "moov" && parent != "trak" {
				return fmt.Errorf("unsupported_metadata_box")
			}
			if err := w.metadata(box, depth+1, track != nil, true); err != nil {
				return err
			}
		case "uuid":
			if parent != "" {
				return fmt.Errorf("unsupported_uuid_box")
			}
			if err := w.uuid(box); err != nil {
				return err
			}
		}
		offset = box.end
	}
	return nil
}

func (w *videoComposeMP4Walker) mediaHandler(box videoComposeMP4Box, parent string, track *videoComposeMP4Track) error {
	if track == nil || box.end-box.payload < 24 {
		return fmt.Errorf("unsupported_sample_description")
	}
	var header [12]byte
	if _, err := w.reader.ReadAt(header[:], box.payload); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(header[:4]) != 0 {
		return fmt.Errorf("unsupported_sample_description")
	}
	handler := string(header[8:12])
	// QuickTime may include a separate minf data handler. These subtypes do
	// not alter FFmpeg's media type; never accept vide/soun in this role.
	if parent == "minf" && string(header[4:8]) == "dhlr" && (handler == "url " || handler == "alis") {
		return nil
	}
	if parent != "mdia" || track.handler != "" || (handler != "vide" && handler != "soun") {
		return fmt.Errorf("unsupported_sample_description")
	}
	track.handler = handler
	return nil
}

func (w *videoComposeMP4Walker) sampleDescriptions(box videoComposeMP4Box, depth int, track *videoComposeMP4Track) error {
	if depth > 6 {
		return fmt.Errorf("MP4 nesting exceeds limit")
	}
	if box.end-box.payload < 8 {
		return fmt.Errorf("invalid_sample_description")
	}
	var header [8]byte
	if _, err := w.reader.ReadAt(header[:], box.payload); err != nil {
		return err
	}
	count := binary.BigEndian.Uint32(header[4:])
	// Version zero gives a single, unambiguous QuickTime audio version
	// branch in n6.1, independent of the file's compatible brands.
	if binary.BigEndian.Uint32(header[:4]) != 0 || count == 0 || count > 1024 || uint64(count) > uint64((box.end-box.payload-8)/16) {
		return fmt.Errorf("invalid_sample_description")
	}
	offset := box.payload + 8
	for i := uint32(0); i < count; i++ {
		entry, err := w.box(offset, box.end)
		if err != nil {
			return err
		}
		var size [4]byte
		if _, err := w.reader.ReadAt(size[:], entry.start); err != nil {
			return err
		}
		// Unlike regular atoms, FFmpeg reads sample-entry size as an
		// ordinary uint32: no zero-to-end or extended-size encoding.
		if entry.header != 8 || binary.BigEndian.Uint32(size[:]) < 16 || entry.end-entry.payload < 8 {
			return fmt.Errorf("invalid_sample_description")
		}
		var common [8]byte
		if _, err := w.reader.ReadAt(common[:], entry.payload); err != nil {
			return err
		}
		if common != [8]byte{0, 0, 0, 0, 0, 0, 0, 1} {
			return fmt.Errorf("external or unsupported MP4 data reference")
		}
		switch entry.kind {
		case "encv", "enca", "mebx", "tmcd":
			return fmt.Errorf("unsupported_sample_description")
		}
		prefix := int64(78)
		if track.handler == "soun" {
			prefix = 28
			if entry.end-entry.payload < prefix {
				return fmt.Errorf("invalid_sample_description")
			}
			var version [2]byte
			if _, err := w.reader.ReadAt(version[:], entry.payload+8); err != nil {
				return err
			}
			switch binary.BigEndian.Uint16(version[:]) {
			case 0:
			case 1:
				prefix += 16
			case 2:
				prefix += 36
			default:
				return fmt.Errorf("unsupported_sample_description")
			}
		}
		if entry.end-entry.payload < prefix {
			return fmt.Errorf("invalid_sample_description")
		}
		if track.handler == "vide" {
			var depth [2]byte
			if _, err := w.reader.ReadAt(depth[:], entry.payload+74); err != nil {
				return err
			}
			// qtpalette can consume a variable color table even for a
			// familiar codec tag. That is not the fixed camera layout.
			switch binary.BigEndian.Uint16(depth[:]) & 31 {
			case 1, 2, 4, 8:
				return fmt.Errorf("unsupported_sample_description")
			}
		}
		// stsd/entry and the optional single wave are finite roles, not
		// extra levels of arbitrary movie/metadata recursion. Keep the
		// depth-six bound and shared box budget for their dispatched atoms.
		if err := w.movie(entry.payload+prefix, entry.end, "sample-entry", depth, track); err != nil {
			return err
		}
		offset = entry.end
	}
	if offset != box.end {
		return fmt.Errorf("invalid_sample_description")
	}
	return nil
}

func (w *videoComposeMP4Walker) wave(box videoComposeMP4Box, depth int, track *videoComposeMP4Track) error {
	if box.end-box.payload < 12 {
		return fmt.Errorf("unsupported_sample_description")
	}
	var first [12]byte
	if _, err := w.reader.ReadAt(first[:], box.payload); err != nil {
		return err
	}
	// n6.1's ALAC compatibility path may otherwise consume eight bytes
	// before invoking the global dispatcher. This exact first frma forces
	// its rewind branch whenever that path applies. Other audio codecs
	// either use this same aligned dispatcher or consume wave opaquely;
	// we conservatively require the structured form for all of them.
	if binary.BigEndian.Uint32(first[:4]) != 12 || string(first[4:8]) != "frma" {
		return fmt.Errorf("unsupported_sample_description")
	}
	return w.movie(box.payload, box.end, "wave", depth, track)
}

func (w *videoComposeMP4Walker) uuid(box videoComposeMP4Box) error {
	if box.end-box.payload < 16 {
		return fmt.Errorf("invalid_uuid_box")
	}
	var id [16]byte
	if _, err := w.reader.ReadAt(id[:], box.payload); err != nil {
		return err
	}
	xmp := [16]byte{0xbe, 0x7a, 0xcf, 0xcb, 0x97, 0xa9, 0x42, 0xe8, 0x9c, 0x71, 0x99, 0x94, 0x91, 0xe3, 0xaf, 0xac}
	spherical := [16]byte{0xff, 0xcc, 0x82, 0x63, 0xf8, 0x55, 0x4a, 0x93, 0x88, 0x14, 0x58, 0x7a, 0x02, 0x52, 0x1f, 0xdd}
	if id != xmp && id != spherical {
		return fmt.Errorf("unsupported_uuid_box")
	}
	if box.end-box.payload == 16 {
		return fmt.Errorf("invalid_uuid_box")
	}
	if err := w.metadataBox(box, true); err != nil {
		return err
	}
	// No payload parser runs in preflight. This does NOT claim that FFmpeg
	// treats the allowed namespace as opaque: spherical metadata is interpreted
	// by the decoder. Real encoded camera acceptance remains a separate gate.
	return nil
}

func (w *videoComposeMP4Walker) metadata(box videoComposeMP4Box, depth int, inTrack, root bool) error {
	if depth > 6 {
		return fmt.Errorf("MP4 nesting exceeds limit")
	}
	if err := w.metadataBox(box, root); err != nil {
		return err
	}
	if box.kind == "udta" {
		for offset := box.payload; offset < box.end; {
			child, err := w.box(offset, box.end)
			if err != nil {
				return err
			}
			if err := w.metadataBox(child, false); err != nil {
				return err
			}
			switch child.kind {
			case "meta", "udta":
				if err := w.metadata(child, depth+1, inTrack, false); err != nil {
					return err
				}
			default:
				if !videoComposeMP4TextMetadata(child.kind) {
					return fmt.Errorf("unsupported_metadata_box")
				}
			}
			offset = child.end
		}
		return nil
	}
	return w.meta(box, depth, inTrack)
}

func (w *videoComposeMP4Walker) meta(box videoComposeMP4Box, depth int, inTrack bool) error {
	start := box.payload
	if box.end-start < 8 {
		return fmt.Errorf("invalid_metadata_box")
	}
	var version [4]byte
	if _, err := w.reader.ReadAt(version[:], start); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(version[:]) == 0 {
		start += 4 // ISO FullBox variant; QuickTime may omit this zero word.
	}
	handler, err := w.box(start, box.end)
	if err != nil {
		return err
	}
	if err := w.metadataBox(handler, false); err != nil {
		return err
	}
	if handler.kind != "hdlr" || handler.end-handler.payload < 24 {
		return fmt.Errorf("invalid_metadata_box")
	}
	var hdlr [12]byte
	if _, err := w.reader.ReadAt(hdlr[:], handler.payload); err != nil {
		return err
	}
	mode := string(hdlr[8:12])
	if binary.BigEndian.Uint32(hdlr[:4]) != 0 || (mode != "mdta" && mode != "mdir") || (inTrack && mode == "mdta") {
		return fmt.Errorf("unsupported_metadata_box")
	}
	// FFmpeg retains only the first file-global mdta key table. Refuse a second
	// mdta handler rather than validate indices against a different local table.
	if mode == "mdta" {
		if w.haveMDTA {
			return fmt.Errorf("unsupported_metadata_box")
		}
		w.haveMDTA = true
	}
	keys, haveKeys, haveList := uint32(0), false, false
	for offset := handler.end; offset < box.end; {
		child, err := w.box(offset, box.end)
		if err != nil {
			return err
		}
		if err := w.metadataBox(child, false); err != nil {
			return err
		}
		switch child.kind {
		case "keys":
			if mode != "mdta" || haveKeys || haveList {
				return fmt.Errorf("unsupported_metadata_box")
			}
			keys, err = w.keys(child, depth+1)
			if err != nil {
				return err
			}
			haveKeys = true
		case "ilst":
			if haveList || (mode == "mdta" && !haveKeys) {
				return fmt.Errorf("invalid_metadata_box")
			}
			if err := w.items(child, depth+1, mode, keys); err != nil {
				return err
			}
			haveList = true
		default:
			return fmt.Errorf("unsupported_metadata_box")
		}
		offset = child.end
	}
	return nil
}

func (w *videoComposeMP4Walker) keys(box videoComposeMP4Box, depth int) (uint32, error) {
	if depth > 6 {
		return 0, fmt.Errorf("MP4 nesting exceeds limit")
	}
	if box.end-box.payload < 8 {
		return 0, fmt.Errorf("invalid_metadata_box")
	}
	var header [8]byte
	if _, err := w.reader.ReadAt(header[:], box.payload); err != nil {
		return 0, err
	}
	count := binary.BigEndian.Uint32(header[4:])
	if binary.BigEndian.Uint32(header[:4]) != 0 || uint64(count) > uint64((box.end-box.payload-8)/9) {
		return 0, fmt.Errorf("invalid_metadata_box")
	}
	offset := box.payload + 8
	for i := uint32(0); i < count; i++ {
		entry, err := w.box(offset, box.end)
		if err != nil {
			return 0, err
		}
		if entry.header != 8 || entry.kind != "mdta" || entry.end-entry.payload < 1 {
			return 0, fmt.Errorf("invalid_metadata_box")
		}
		// Key entries are ordinary size-prefixed records, not ISO boxes that
		// can use the special zero-to-parent or extended-size encodings.
		var size [4]byte
		if _, err := w.reader.ReadAt(size[:], entry.start); err != nil {
			return 0, err
		}
		if binary.BigEndian.Uint32(size[:]) < 9 {
			return 0, fmt.Errorf("invalid_metadata_box")
		}
		name := make([]byte, int(entry.end-entry.payload))
		if _, err := w.reader.ReadAt(name, entry.payload); err != nil {
			return 0, err
		}
		if !utf8.Valid(name) || strings.IndexByte(string(name), 0) >= 0 || string(name) == "com.apple.quicktime.artwork" {
			return 0, fmt.Errorf("unsupported_metadata_box")
		}
		offset = entry.end
	}
	if offset != box.end {
		return 0, fmt.Errorf("invalid_metadata_box")
	}
	return count, nil
}

func (w *videoComposeMP4Walker) items(box videoComposeMP4Box, depth int, mode string, keys uint32) error {
	if depth > 6 {
		return fmt.Errorf("MP4 nesting exceeds limit")
	}
	for offset := box.payload; offset < box.end; {
		item, err := w.box(offset, box.end)
		if err != nil {
			return err
		}
		if err := w.metadataBox(item, false); err != nil {
			return err
		}
		if mode == "mdta" {
			index := binary.BigEndian.Uint32([]byte(item.kind))
			if index == 0 || index > keys {
				return fmt.Errorf("unsupported_metadata_box")
			}
		} else if !videoComposeMP4TextMetadata(item.kind) {
			return fmt.Errorf("unsupported_metadata_box")
		}
		if depth+1 > 6 {
			return fmt.Errorf("MP4 nesting exceeds limit")
		}
		data, err := w.box(item.payload, item.end)
		if err != nil {
			return err
		}
		if err := w.metadataBox(data, false); err != nil {
			return err
		}
		if data.kind != "data" {
			return fmt.Errorf("unsupported_metadata_box")
		}
		if data.end != item.end || data.end-data.payload < 8 {
			return fmt.Errorf("invalid_metadata_box")
		}
		offset = item.end
	}
	return nil
}

func videoComposeMP4TextMetadata(kind string) bool {
	// These are known MOV string leaves, not global movie/reference parsers.
	// In particular, n6.1's udta string reader treats swr/enc as encoder,
	// mak/mod as device strings and xyz as location text. Android's writer
	// serializes xyz as ISO-6709 coordinates, not a dereferenceable location.
	// Do not accept arbitrary copyright-prefixed names, artwork, URLs or
	// unreviewed vendor structures merely because their payload looks like text.
	switch kind {
	case "\xa9nam", "\xa9ART", "\xa9alb", "\xa9day", "\xa9too", "\xa9cmt", "\xa9gen", "\xa9swr", "\xa9mak", "\xa9mod", "\xa9enc", "\xa9xyz", "cprt", "desc":
		return true
	default:
		return false
	}
}
