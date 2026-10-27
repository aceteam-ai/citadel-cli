package cmd

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// These ordinary-container fixtures use roles whose payloads are box lists.
// They do not invent codec sample-entry offsets or claim to be
// playable camera recordings. The ordinary data reference stays valid so each
// refusal is attributable to the added descendant, not a missing track anchor.
func cameraMP4OrdinaryContainerFixture(t *testing.T, parent string, additions ...[]byte) []byte {
	t.Helper()
	switch parent {
	case "mdia", "minf", "dinf", "stbl", "edts", "tref":
	default:
		t.Fatal("unknown synthetic ordinary-container role", parent)
	}
	ref := videoComposeTestBox("url ", []byte{0, 0, 0, 1})
	dref := videoComposeTestBox("dref", []byte{0, 0, 0, 0, 0, 0, 0, 1}, ref)
	dinf := [][]byte{dref}
	if parent == "dinf" {
		dinf = append(dinf, additions...)
	}
	minf := [][]byte{videoComposeTestBox("dinf", dinf...)}
	if parent == "minf" {
		minf = append(minf, additions...)
	}
	if parent == "stbl" {
		minf = append(minf, videoComposeTestBox("stbl", additions...))
	}
	mdia := [][]byte{videoComposeTestBox("minf", minf...)}
	if parent == "mdia" {
		mdia = append(mdia, additions...)
	}
	track := [][]byte{videoComposeTestBox("mdia", mdia...)}
	if parent == "edts" {
		track = append(track, videoComposeTestBox("edts", additions...))
	}
	if parent == "tref" {
		track = append(track, videoComposeTestBox("tref", additions...))
	}
	body := videoComposeTestBox("ftyp", []byte("qt  \x00\x00\x00\x00isommp41"))
	body = append(body, videoComposeTestBox("moov", videoComposeTestBox("trak", track...))...)
	return append(body, videoComposeTestBox("mdat", []byte{0})...)
}

func TestVideoComposeCameraMetadataCannotHideInDispatchedContainers(t *testing.T) {
	external := videoComposeTestBox("dref", []byte{0, 0, 0, 0, 0, 0, 0, 1}, videoComposeTestBox("url ", []byte("external-camera-path.mp4")))
	for _, test := range []struct {
		name   string
		parent string
		child  []byte
		want   string
	}{
		{name: "stbl udta external reference", parent: "stbl", child: videoComposeTestBox("udta", external), want: "unsupported_metadata_box"},
		{name: "stbl ISML tfxd uuid", parent: "stbl", child: cameraMP4UUID(t, "6d1d9b0542d544e680e2141daff757b2", []byte{0}, false), want: "unsupported_uuid_box"},
		{name: "stbl nested track", parent: "stbl", child: videoComposeTestBox("trak"), want: "unexpected MP4 container"},
		{name: "edts udta external reference", parent: "edts", child: videoComposeTestBox("udta", external), want: "unsupported_metadata_box"},
		{name: "tref udta external reference", parent: "tref", child: videoComposeTestBox("udta", external), want: "unsupported_metadata_box"},
		{name: "stbl malformed meta", parent: "stbl", child: videoComposeTestBox("meta", []byte("garbage!")), want: "unsupported_metadata_box"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := cameraMP4Validate(t, cameraMP4OrdinaryContainerFixture(t, test.parent)); err != nil {
				t.Fatal("baseline ordinary-container fixture refused before injection", err)
			}
			cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, test.parent, test.child), test.want)
		})
	}
}

func TestVideoComposeCameraRejectsChapterReferencesAtEveryWalkedRole(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(key, root)
	}
	// A complete 12-byte chapter-reference leaf pointing at track one.
	// These are structural fixtures, not encoded or playable camera media.
	chap := videoComposeTestBox("chap", []byte{0, 0, 0, 1})
	for _, parent := range []string{"mdia", "minf", "dinf", "stbl", "edts", "tref"} {
		t.Run(parent, func(t *testing.T) {
			if err := cameraMP4Validate(t, cameraMP4OrdinaryContainerFixture(t, parent)); err != nil {
				t.Fatal("ordinary-container control refused before chapter reference", err)
			}
			cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, parent, chap), "unsupported_chapter_reference")
		})
	}
	for _, parent := range []string{"top", "moov", "trak"} {
		t.Run(parent, func(t *testing.T) {
			if err := cameraMP4Validate(t, cameraMP4Fixture("qt  ", parent)); err != nil {
				t.Fatal("movie-role control refused before chapter reference", err)
			}
			cameraMP4RequireReason(t, cameraMP4Fixture("qt  ", parent, chap), "unsupported_chapter_reference")
		})
	}
	for _, handler := range []string{"vide", "soun"} {
		t.Run(handler+" sample entry", func(t *testing.T) {
			entry := func(children ...[]byte) []byte { return cameraMP4VisualEntry("avc1", children...) }
			if handler == "soun" {
				entry = func(children ...[]byte) []byte { return cameraMP4AudioEntry("mp4a", 0, children...) }
			}
			if err := cameraMP4Validate(t, cameraMP4SampleFixture(handler, cameraMP4STSD(entry()))); err != nil {
				t.Fatal("sample-entry control refused before chapter reference", err)
			}
			cameraMP4RequireReason(t, cameraMP4SampleFixture(handler, cameraMP4STSD(entry(chap))), "unsupported_chapter_reference")
		})
	}
	t.Run("audio wave", func(t *testing.T) {
		frma := videoComposeTestBox("frma", []byte("mp4a"))
		baseline := cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("mp4a", 0, videoComposeTestBox("wave", frma))))
		if err := cameraMP4Validate(t, baseline); err != nil {
			t.Fatal("aligned wave control refused before chapter reference", err)
		}
		poisoned := cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("mp4a", 0, videoComposeTestBox("wave", frma, chap))))
		cameraMP4RequireReason(t, poisoned, "unsupported_chapter_reference")
	})
	// Opaque text payloads are not dispatched box lists. This guard rejects
	// structural roles, without byte-scanning harmless lookalike text.
	text := videoComposeTestBox("udta", videoComposeTestBox("\xa9mak", chap))
	if err := cameraMP4Validate(t, cameraMP4Fixture("qt  ", "moov", text)); err != nil {
		t.Fatal("opaque reviewed text was scanned for chapter-reference bytes", err)
	}
}

func TestVideoComposeCameraMetadataParentGateCoversEveryWalkedTrackRole(t *testing.T) {
	for _, parent := range []string{"mdia", "minf", "dinf"} {
		for _, kind := range []string{"meta", "udta"} {
			t.Run(parent+"/"+kind, func(t *testing.T) {
				metadata := videoComposeTestBox("udta", videoComposeTestBox("\xa9too", []byte("Camera recorder")))
				if kind == "meta" {
					metadata = cameraMP4MDIR(videoComposeTestBox("\xa9nam", cameraMP4Data([]byte("Camera chapter"))))
				}
				// The exact same complete metadata is valid at an approved role;
				// malformed metadata cannot accidentally satisfy this gate test.
				if err := cameraMP4Validate(t, cameraMP4Fixture("qt  ", "top", metadata)); err != nil {
					t.Fatal("approved-role control metadata refused", err)
				}
				cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, parent, metadata), "unsupported_metadata_box")
			})
		}
	}
}

func TestVideoComposeCameraHiddenMetadataCannotEscapeByteBudgets(t *testing.T) {
	// A forbidden ordinary-container parent refuses before metadata admission.
	// These cases must not become opaque accepted bytes merely because their
	// metadata also exceeds a per-box or aggregate budget.
	tooLarge := videoComposeTestBox("udta", videoComposeTestBox("\xa9too", bytes.Repeat([]byte{'A'}, 2<<20)))
	cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, "stbl", tooLarge), "unsupported_metadata_box")
	roots := make([][]byte, 5)
	for i := range roots {
		roots[i] = videoComposeTestBox("udta", videoComposeTestBox("\xa9too", bytes.Repeat([]byte{'B'}, (1<<20)-16)))
		if len(roots[i]) != 1<<20 {
			t.Fatal("hidden metadata root is not exactly one MiB")
		}
	}
	cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, "stbl", roots...), "unsupported_metadata_box")
}

func TestVideoComposeCameraReviewedQuickTimeTextLeaves(t *testing.T) {
	for _, kind := range []string{"\xa9swr", "\xa9mak", "\xa9mod", "\xa9enc", "\xa9xyz"} {
		for _, placement := range []string{"top", "moov", "trak"} {
			t.Run(kind+"/"+placement, func(t *testing.T) {
				text := videoComposeTestBox(kind, []byte("Synthetic camera metadata"))
				if err := cameraMP4Validate(t, cameraMP4Fixture("qt  ", placement, videoComposeTestBox("udta", text))); err != nil {
					t.Fatal("reviewed direct QuickTime text leaf refused", err)
				}
				item := videoComposeTestBox(kind, cameraMP4Data([]byte("Synthetic camera metadata")))
				if err := cameraMP4Validate(t, cameraMP4Fixture("qt  ", placement, cameraMP4MDIR(item))); err != nil {
					t.Fatal("reviewed mdir text item refused", err)
				}
			})
		}
	}
}

func TestVideoComposeCameraReviewedTextPayloadIsNotAContainerRole(t *testing.T) {
	lookalike := videoComposeTestBox("dref", videoComposeTestBox("url ", []byte("synthetic-external-path.mp4")))
	for _, kind := range []string{"\xa9swr", "\xa9mak", "\xa9mod", "\xa9enc", "\xa9xyz"} {
		t.Run(kind, func(t *testing.T) {
			// Text namespace admission is bounded and opaque in preflight.
			// Bytes resembling a container do not change a reviewed text leaf
			// into a recursive box-list role or prove native text validity.
			metadata := videoComposeTestBox("udta", videoComposeTestBox(kind, lookalike))
			if err := cameraMP4Validate(t, cameraMP4Fixture("qt  ", "moov", metadata)); err != nil {
				t.Fatal("text leaf was recursively interpreted as an external reference", err)
			}
			item := videoComposeTestBox(kind, cameraMP4Data(lookalike))
			if err := cameraMP4Validate(t, cameraMP4Fixture("qt  ", "moov", cameraMP4MDIR(item))); err != nil {
				t.Fatal("text data bytes were recursively interpreted", err)
			}
		})
	}
}

func cameraMP4TrackMediaFixture(media ...[]byte) []byte {
	body := videoComposeTestBox("ftyp", []byte("qt  \x00\x00\x00\x00isommp41"))
	body = append(body, videoComposeTestBox("moov", videoComposeTestBox("trak", videoComposeTestBox("mdia", media...)))...)
	return append(body, videoComposeTestBox("mdat", []byte{0})...)
}

func cameraMP4SampleMinf(descriptions ...[]byte) []byte {
	ref := videoComposeTestBox("url ", []byte{0, 0, 0, 1})
	dref := videoComposeTestBox("dref", []byte{0, 0, 0, 0, 0, 0, 0, 1}, ref)
	return videoComposeTestBox("minf", videoComposeTestBox("dinf", dref), videoComposeTestBox("stbl", descriptions...))
}

func cameraMP4SampleFixture(handler string, descriptions ...[]byte) []byte {
	return cameraMP4TrackMediaFixture(cameraMP4Handler(handler), cameraMP4SampleMinf(descriptions...))
}

func cameraMP4STSD(entries ...[]byte) []byte {
	header := make([]byte, 8) // FullBox version/flags zero, entry count.
	binary.BigEndian.PutUint32(header[4:8], uint32(len(entries)))
	return videoComposeTestBox("stsd", append([][]byte{header}, entries...)...)
}

// Fixed prefixes follow the reviewed n6.1 MOV visual and QuickTime-audio
// layouts. Only structural extension admission is tested, not decodable media.
func cameraMP4VisualEntry(kind string, extensions ...[]byte) []byte {
	prefix := make([]byte, 78)
	binary.BigEndian.PutUint16(prefix[6:8], 1) // Same local data-reference index.
	binary.BigEndian.PutUint16(prefix[24:26], 1920)
	binary.BigEndian.PutUint16(prefix[26:28], 1080)
	binary.BigEndian.PutUint32(prefix[28:32], 72<<16)
	binary.BigEndian.PutUint32(prefix[32:36], 72<<16)
	binary.BigEndian.PutUint16(prefix[40:42], 1)
	binary.BigEndian.PutUint16(prefix[74:76], 24) // No variable palette to skip.
	binary.BigEndian.PutUint16(prefix[76:78], 0xffff)
	return videoComposeTestBox(kind, append([][]byte{prefix}, extensions...)...)
}

func cameraMP4AudioEntry(kind string, version uint16, extensions ...[]byte) []byte {
	size := 28
	if version == 1 {
		size += 16
	} else if version == 2 {
		size += 36
	}
	prefix := make([]byte, size)
	binary.BigEndian.PutUint16(prefix[6:8], 1)
	binary.BigEndian.PutUint16(prefix[8:10], version)
	binary.BigEndian.PutUint16(prefix[16:18], 2)
	binary.BigEndian.PutUint16(prefix[18:20], 16)
	binary.BigEndian.PutUint32(prefix[24:28], 48000<<16)
	if version == 1 {
		// Benign PCM field values from the pinned QuickTime writer layout.
		binary.BigEndian.PutUint32(prefix[28:32], 1)
		binary.BigEndian.PutUint32(prefix[32:36], 2)
		binary.BigEndian.PutUint32(prefix[36:40], 4)
		binary.BigEndian.PutUint32(prefix[40:44], 2)
	} else if version == 2 {
		binary.BigEndian.PutUint16(prefix[16:18], 3)
		binary.BigEndian.PutUint16(prefix[20:22], 0xfffe)
		binary.BigEndian.PutUint32(prefix[24:28], 0x00010000)
		binary.BigEndian.PutUint32(prefix[28:32], 72)
		binary.BigEndian.PutUint64(prefix[32:40], 0x40e7700000000000) // IEEE754 48000.
		binary.BigEndian.PutUint32(prefix[40:44], 2)
		binary.BigEndian.PutUint32(prefix[44:48], 0x7f000000)
		binary.BigEndian.PutUint32(prefix[48:52], 16)
		binary.BigEndian.PutUint32(prefix[56:60], 4)
		binary.BigEndian.PutUint32(prefix[60:64], 1)
	}
	return videoComposeTestBox(kind, append([][]byte{prefix}, extensions...)...)
}

func TestVideoComposeCameraSampleDescriptionStructuralProfiles(t *testing.T) {
	for _, kind := range []string{"avc1", "hvc1", "mp4v", "apcn"} {
		t.Run("vide/"+kind, func(t *testing.T) {
			if err := cameraMP4Validate(t, cameraMP4SampleFixture("vide", cameraMP4STSD(cameraMP4VisualEntry(kind)))); err != nil {
				t.Fatal("fixed non-paletted visual layout refused", err)
			}
		})
	}
	for _, kind := range []string{"mp4a", "lpcm", "alac"} {
		for _, version := range []uint16{0, 1, 2} {
			t.Run("soun/"+kind+"/"+string(rune('0'+version)), func(t *testing.T) {
				entry := cameraMP4AudioEntry(kind, version)
				if err := cameraMP4Validate(t, cameraMP4SampleFixture("soun", cameraMP4STSD(entry))); err != nil {
					t.Fatal("fixed QuickTime audio version layout refused", err)
				}
			})
		}
	}
}

func TestVideoComposeCameraSampleExtensionsCannotHideDispatchedRoles(t *testing.T) {
	for _, test := range []struct {
		kind string
		body []byte
		want string
	}{
		{kind: "udta", body: videoComposeTestBox("udta", videoComposeTestBox("dref")), want: "unsupported_metadata_box"},
		{kind: "meta", body: cameraMP4MDIR(videoComposeTestBox("\xa9nam", cameraMP4Data([]byte("hidden")))), want: "unsupported_metadata_box"},
		{kind: "uuid", body: cameraMP4UUID(t, "6d1d9b0542d544e680e2141daff757b2", []byte{0}, false), want: "unsupported_uuid_box"},
		{kind: "dref", body: videoComposeTestBox("dref", make([]byte, 20)), want: "external or unsupported MP4 data reference"},
		{kind: "url", body: videoComposeTestBox("url ", []byte("external.mp4")), want: "external or unsupported MP4 data reference"},
		{kind: "trak", body: videoComposeTestBox("trak"), want: "unexpected MP4 container"},
		{kind: "mdat", body: videoComposeTestBox("mdat", []byte{0}), want: "unexpected MP4 container"},
		{kind: "stsd", body: cameraMP4STSD(cameraMP4VisualEntry("avc1")), want: "unsupported_sample_description"},
		{kind: "hdlr", body: cameraMP4Handler("soun"), want: "unsupported_sample_description"},
		{kind: "sinf", body: videoComposeTestBox("sinf"), want: "unsupported MP4 container"},
	} {
		for _, profile := range []struct {
			handler string
			version uint16
		}{{handler: "vide"}, {handler: "soun"}, {handler: "soun", version: 1}, {handler: "soun", version: 2}} {
			t.Run(profile.handler+"/"+string(rune('0'+profile.version))+"/"+test.kind, func(t *testing.T) {
				entry := cameraMP4VisualEntry("avc1")
				injected := cameraMP4VisualEntry("avc1", test.body)
				if profile.handler == "soun" {
					entry = cameraMP4AudioEntry("mp4a", profile.version)
					injected = cameraMP4AudioEntry("mp4a", profile.version, test.body)
				}
				if err := cameraMP4Validate(t, cameraMP4SampleFixture(profile.handler, cameraMP4STSD(entry))); err != nil {
					t.Fatal("sample extension baseline refused before hostile injection", err)
				}
				cameraMP4RequireReason(t, cameraMP4SampleFixture(profile.handler, cameraMP4STSD(injected)), test.want)
			})
		}
	}
}

func TestVideoComposeCameraSTSDRequiresUnambiguousHandlerAndFraming(t *testing.T) {
	valid := cameraMP4STSD(cameraMP4VisualEntry("avc1"))
	for _, fixture := range [][]byte{
		cameraMP4TrackMediaFixture(cameraMP4SampleMinf(valid)),
		cameraMP4TrackMediaFixture(cameraMP4SampleMinf(valid), cameraMP4Handler("vide")),
		cameraMP4TrackMediaFixture(cameraMP4Handler("vide"), cameraMP4Handler("vide"), cameraMP4SampleMinf(valid)),
		cameraMP4SampleFixture("meta", valid),
		cameraMP4SampleFixture("vide", valid, valid),
	} {
		cameraMP4RequireReason(t, fixture, "unsupported_sample_description")
	}
	flags := bytes.Clone(valid)
	flags[11] = 1
	trailing := videoComposeTestBox("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, cameraMP4VisualEntry("avc1"), []byte{0})
	zero := cameraMP4VisualEntry("avc1")
	binary.BigEndian.PutUint32(zero[:4], 0)
	extended := make([]byte, 16)
	binary.BigEndian.PutUint32(extended[:4], 1)
	copy(extended[4:8], "avc1")
	binary.BigEndian.PutUint64(extended[8:16], uint64(len(extended)+78))
	extended = append(extended, cameraMP4VisualEntry("avc1")[8:]...)
	for _, description := range [][]byte{flags, trailing, cameraMP4STSD(), cameraMP4STSD(zero), cameraMP4STSD(extended), cameraMP4STSD(videoComposeTestBox("avc1", make([]byte, 7)))} {
		cameraMP4RequireReason(t, cameraMP4SampleFixture("vide", description), "invalid_sample_description")
	}
	for _, depth := range []uint16{1, 2, 4, 8} {
		entry := cameraMP4VisualEntry("avc1")
		binary.BigEndian.PutUint16(entry[8+74:8+76], depth)
		cameraMP4RequireReason(t, cameraMP4SampleFixture("vide", cameraMP4STSD(entry)), "unsupported_sample_description")
	}
	for _, kind := range []string{"encv", "enca", "mebx", "tmcd"} {
		cameraMP4RequireReason(t, cameraMP4SampleFixture("vide", cameraMP4STSD(cameraMP4VisualEntry(kind))), "unsupported_sample_description")
	}
	for _, index := range []uint16{0, 2} {
		entry := cameraMP4VisualEntry("avc1")
		binary.BigEndian.PutUint16(entry[8+6:8+8], index)
		cameraMP4RequireReason(t, cameraMP4SampleFixture("vide", cameraMP4STSD(entry)), "external or unsupported MP4 data reference")
	}
	for _, version := range []uint16{0, 1, 2} {
		entry := cameraMP4AudioEntry("mp4a", version)
		entry = entry[:len(entry)-1]
		binary.BigEndian.PutUint32(entry[:4], uint32(len(entry)))
		cameraMP4RequireReason(t, cameraMP4SampleFixture("soun", cameraMP4STSD(entry)), "invalid_sample_description")
	}
	cameraMP4RequireReason(t, cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("mp4a", 3))), "unsupported_sample_description")
}

func TestVideoComposeCameraAudioWaveExtensionsUseFramedRoles(t *testing.T) {
	frma := videoComposeTestBox("frma", []byte("mp4a"))
	wave := videoComposeTestBox("wave", frma)
	for _, kind := range []string{"mp4a", "lpcm", "alac"} {
		for _, version := range []uint16{0, 1, 2} {
			if err := cameraMP4Validate(t, cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry(kind, version, wave)))); err != nil {
				t.Fatal("frma-aligned QuickTime audio wave fixture refused", err)
			}
		}
	}
	// FFmpeg's ALAC compatibility branch starts at a 24-byte wave payload.
	// Both variants reach that size; only the correctly aligned first frma is
	// admitted, rather than relying on an unrelated short-payload refusal.
	alacFormat := videoComposeTestBox("frma", []byte("alac"))
	padding := videoComposeTestBox("free", make([]byte, 4))
	alacWave := videoComposeTestBox("wave", alacFormat, padding)
	if err := cameraMP4Validate(t, cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("alac", 0, alacWave)))); err != nil {
		t.Fatal("frma-aligned 24-byte ALAC wave control refused", err)
	}
	wrongALACWave := videoComposeTestBox("wave", padding, alacFormat)
	cameraMP4RequireReason(t, cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("alac", 0, wrongALACWave))), "unsupported_sample_description")
	cameraMP4RequireReason(t, cameraMP4SampleFixture("vide", cameraMP4STSD(cameraMP4VisualEntry("avc1", wave))), "unsupported MP4 container")
	for _, bad := range [][]byte{
		videoComposeTestBox("wave"),
		videoComposeTestBox("wave", videoComposeTestBox("frma", []byte("aac"))),
		videoComposeTestBox("wave", videoComposeTestBox("frma", []byte("mp4aa"))),
		videoComposeTestBox("wave", videoComposeTestBox("free", make([]byte, 4)), frma),
	} {
		cameraMP4RequireReason(t, cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("mp4a", 0, bad))), "unsupported_sample_description")
	}
	for _, test := range []struct {
		child []byte
		want  string
	}{
		{videoComposeTestBox("udta", videoComposeTestBox("dref")), "unsupported_metadata_box"},
		{cameraMP4UUID(t, cameraMP4XMPUUID, []byte{0}, false), "unsupported_uuid_box"},
		{videoComposeTestBox("dref", make([]byte, 20)), "external or unsupported MP4 data reference"},
		{videoComposeTestBox("trak"), "unexpected MP4 container"},
		{cameraMP4STSD(cameraMP4AudioEntry("mp4a", 0)), "unsupported_sample_description"},
		{videoComposeTestBox("wave", frma), "unsupported MP4 container"},
	} {
		bad := videoComposeTestBox("wave", frma, test.child)
		for _, version := range []uint16{0, 1, 2} {
			cameraMP4RequireReason(t, cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("mp4a", version, bad))), test.want)
		}
	}
}

func TestVideoComposeCameraDispatchedContainerRolesRemainClosed(t *testing.T) {
	for _, kind := range []string{"moof", "mvex", "traf", "sinf", "schi", "hoov"} {
		cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, "stbl", videoComposeTestBox(kind)), "unsupported MP4 container")
	}
	for _, kind := range []string{"ftyp", "mdat", "wide"} {
		cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, "stbl", videoComposeTestBox(kind)), "unexpected MP4 container")
	}
	for _, kind := range []string{"url ", "urn "} {
		cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, "stbl", videoComposeTestBox(kind, []byte("external.mp4"))), "external or unsupported MP4 data reference")
	}
	for _, kind := range []string{"ilst", "keys"} {
		cameraMP4RequireReason(t, cameraMP4OrdinaryContainerFixture(t, "stbl", videoComposeTestBox(kind)), "unsupported_metadata_box")
	}
}

func TestVideoComposeCameraSampleExtensionsShareGlobalBoxCount(t *testing.T) {
	if err := cameraMP4Validate(t, cameraMP4SampleFixture("vide", cameraMP4STSD(cameraMP4VisualEntry("avc1")))); err != nil {
		t.Fatal("count-limit control refused before extension insertion", err)
	}
	children := make([][]byte, 4100)
	for i := range children {
		children[i] = videoComposeTestBox("free")
	}
	cameraMP4RequireReason(t, cameraMP4SampleFixture("vide", cameraMP4STSD(cameraMP4VisualEntry("avc1", children...))), "boxes")
}

func TestVideoComposeCameraSampleEntryCountHasItsOwnBound(t *testing.T) {
	entries := make([][]byte, 1025)
	for i := range entries {
		entries[i] = cameraMP4VisualEntry("avc1")
	}
	// Every entry is complete and the total box count is below 4096, so the
	// 1025 refusal cannot be satisfied by missing bodies or the global cap.
	if err := cameraMP4Validate(t, cameraMP4SampleFixture("vide", cameraMP4STSD(entries[:1024]...))); err != nil {
		t.Fatal("exact 1024-entry description refused", err)
	}
	cameraMP4RequireReason(t, cameraMP4SampleFixture("vide", cameraMP4STSD(entries...)), "invalid_sample_description")
}

func TestVideoComposeCameraQuickTimeDataHandlerDoesNotChangeMediaRole(t *testing.T) {
	ref := videoComposeTestBox("url ", []byte{0, 0, 0, 1})
	dref := videoComposeTestBox("dref", []byte{0, 0, 0, 0, 0, 0, 0, 1}, ref)
	description := cameraMP4STSD(cameraMP4VisualEntry("avc1"))
	for _, kind := range []string{"url ", "alis"} {
		handler := cameraMP4Handler(kind)
		copy(handler[8+4:8+8], "dhlr")
		minf := videoComposeTestBox("minf", handler, videoComposeTestBox("dinf", dref), videoComposeTestBox("stbl", description))
		if err := cameraMP4Validate(t, cameraMP4TrackMediaFixture(cameraMP4Handler("vide"), minf)); err != nil {
			t.Fatal("QuickTime data handler confused with a replacement media handler", err)
		}
	}
	for _, kind := range []string{"vide", "soun"} {
		handler := cameraMP4Handler(kind)
		copy(handler[8+4:8+8], "dhlr")
		minf := videoComposeTestBox("minf", handler, videoComposeTestBox("dinf", dref), videoComposeTestBox("stbl", description))
		cameraMP4RequireReason(t, cameraMP4TrackMediaFixture(cameraMP4Handler("vide"), minf), "unsupported_sample_description")
	}
}

func cameraMP4WaveFormatAtom() []byte {
	// A complete PCM WAVEFORMATEX header, not a malformed payload used as a
	// substitute for testing the wfex callback's codec-type mutation.
	header := make([]byte, 18)
	binary.LittleEndian.PutUint16(header[0:2], 1)
	binary.LittleEndian.PutUint16(header[2:4], 2)
	binary.LittleEndian.PutUint32(header[4:8], 48000)
	binary.LittleEndian.PutUint32(header[8:12], 192000)
	binary.LittleEndian.PutUint16(header[12:14], 4)
	binary.LittleEndian.PutUint16(header[14:16], 16)
	return videoComposeTestBox("wfex", header)
}

func cameraMP4VisualEntryWithHiddenAudioRole(t *testing.T) []byte {
	t.Helper()
	entry := cameraMP4VisualEntry("avc1")
	reference := videoComposeTestBox("dref", []byte{0, 0, 0, 0, 0, 0, 0, 1}, videoComposeTestBox("url ", []byte{0, 0, 0, 0, 'x', 0}))
	hidden := videoComposeTestBox("udta", reference)
	if len(hidden) > 74-28 {
		t.Fatal("hidden extension does not fit before the visual depth field")
	}
	// The reviewed visual layout skips these fixed fields. An AUDIO mutation
	// instead dispatches from byte 28 and sees the hidden metadata role.
	copy(entry[8+28:8+74], hidden)
	return entry
}

func TestVideoComposeCameraWFEXCannotChangeTheSampleEntryOffset(t *testing.T) {
	wfex := cameraMP4WaveFormatAtom()
	hidden := cameraMP4VisualEntryWithHiddenAudioRole(t)
	description := cameraMP4STSD(hidden)
	control := cameraMP4SampleFixture("vide", description)
	if err := cameraMP4Validate(t, control); err != nil {
		t.Fatal("same visual grammar refused without the type-mutating callback", err)
	}
	before := cameraMP4TrackMediaFixture(cameraMP4Handler("vide"), wfex, cameraMP4SampleMinf(description))
	cameraMP4RequireReason(t, before, "unsupported_sample_description")
	first, poisonedFirst := cameraMP4VisualEntry("avc1"), cameraMP4VisualEntry("avc1", wfex)
	if err := cameraMP4Validate(t, cameraMP4SampleFixture("vide", cameraMP4STSD(first, hidden))); err != nil {
		t.Fatal("multi-entry visual grammar refused before wfex insertion", err)
	}
	// A mutation in one entry can affect a later entry, so rejecting only a
	// mismatched current prefix would not cover this nonlocal dispatch path.
	cameraMP4RequireReason(t, cameraMP4SampleFixture("vide", cameraMP4STSD(poisonedFirst, hidden)), "unsupported_sample_description")
	if err := cameraMP4Validate(t, cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("mp4a", 0)))); err != nil {
		t.Fatal("ordinary audio layout refused before wfex insertion", err)
	}
	cameraMP4RequireReason(t, cameraMP4SampleFixture("soun", cameraMP4STSD(cameraMP4AudioEntry("mp4a", 0, wfex))), "unsupported_sample_description")
}
