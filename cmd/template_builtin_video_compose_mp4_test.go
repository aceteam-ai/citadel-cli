package cmd

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These are synthetic container fixtures, not encoded camera media. They test
// only the bounded preflight grammar and never discover or run media tools.
const cameraMP4XMPUUID = "be7acfcb97a942e89c71999491e3afac"
const cameraMP4SphericalUUID = "ffcc8263f8554a938814587a02521fdd"

func cameraMP4Fixture(brand, placement string, metadata ...[]byte) []byte {
	ref := videoComposeTestBox("url ", []byte{0, 0, 0, 1})
	dref := videoComposeTestBox("dref", []byte{0, 0, 0, 0, 0, 0, 0, 1}, ref)
	media := videoComposeTestBox("mdia", videoComposeTestBox("minf", videoComposeTestBox("dinf", dref)))
	trackParts := [][]byte{media}
	if placement == "trak" {
		trackParts = append(trackParts, metadata...)
	}
	tracks := [][]byte{videoComposeTestBox("trak", trackParts...), videoComposeTestBox("trak", media)}
	if placement == "moov" {
		tracks = append(tracks, metadata...)
	}
	body := videoComposeTestBox("ftyp", []byte(brand+"\x00\x00\x00\x00isommp41"))
	body = append(body, videoComposeTestBox("moov", tracks...)...)
	body = append(body, videoComposeTestBox("mdat", []byte{0})...)
	if placement == "top" {
		for _, item := range metadata {
			body = append(body, item...)
		}
	}
	return body
}

func cameraMP4Handler(handler string) []byte {
	payload := make([]byte, 24) // version/flags, pre_defined, handler, reserved.
	copy(payload[8:12], handler)
	return videoComposeTestBox("hdlr", payload)
}

func cameraMP4Keys(names ...string) []byte {
	payload := make([]byte, 8) // FullBox zero, entry count.
	binary.BigEndian.PutUint32(payload[4:8], uint32(len(names)))
	for _, name := range names {
		payload = append(payload, videoComposeTestBox("mdta", []byte(name))...)
	}
	return videoComposeTestBox("keys", payload)
}

func cameraMP4Data(value []byte) []byte {
	payload := make([]byte, 8) // UTF-8 data type 1, locale zero.
	binary.BigEndian.PutUint32(payload[:4], 1)
	return videoComposeTestBox("data", payload, value)
}

func cameraMP4NumericItem(index uint32, children ...[]byte) []byte {
	var kind [4]byte
	binary.BigEndian.PutUint32(kind[:], index)
	return videoComposeTestBox(string(kind[:]), children...)
}

func cameraMP4MDTA(fullBox bool, extra ...[]byte) []byte {
	parts := [][]byte{cameraMP4Handler("mdta"), cameraMP4Keys("com.apple.quicktime.description"),
		videoComposeTestBox("ilst", cameraMP4NumericItem(1, cameraMP4Data([]byte("Camera chapter"))))}
	if fullBox {
		parts = append([][]byte{make([]byte, 4)}, parts...)
	}
	parts = append(parts, extra...)
	return videoComposeTestBox("meta", parts...)
}

func cameraMP4MDIR(children ...[]byte) []byte {
	return videoComposeTestBox("meta", make([]byte, 4), cameraMP4Handler("mdir"), videoComposeTestBox("ilst", children...))
}

func cameraMP4UUID(t *testing.T, namespace string, payload []byte, extended bool) []byte {
	t.Helper()
	uuid, err := hex.DecodeString(namespace)
	if err != nil || len(uuid) != 16 {
		t.Fatal("invalid fixture UUID")
	}
	if !extended {
		return videoComposeTestBox("uuid", uuid, payload)
	}
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[:4], 1)
	copy(header[4:8], "uuid")
	binary.BigEndian.PutUint64(header[8:16], uint64(32+len(payload)))
	return append(append(header, uuid...), payload...)
}

func cameraMP4SizedUUID(t *testing.T, total int, extended bool) []byte {
	t.Helper()
	header := 24
	if extended {
		header = 32
	}
	if total <= header {
		t.Fatal("fixture size must include UUID header and nonempty body")
	}
	return cameraMP4UUID(t, cameraMP4XMPUUID, make([]byte, total-header), extended)
}

func cameraMP4NestedUDTA(depth int, leaf []byte) []byte {
	for i := 0; i < depth; i++ {
		leaf = videoComposeTestBox("udta", leaf)
	}
	return leaf
}

func cameraMP4Validate(t *testing.T, body []byte) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "synthetic-camera.mp4")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return validateVideoComposeMP4(path)
}

func cameraMP4RequireReason(t *testing.T, body []byte, reason string) {
	t.Helper()
	if err := cameraMP4Validate(t, body); err == nil || !strings.Contains(err.Error(), reason) {
		t.Fatalf("wanted precise refusal %q, got %v", reason, err)
	}
}

func TestVideoComposeCameraMetadataProfiles(t *testing.T) {
	for _, test := range []struct {
		name      string
		placement string
		metadata  []byte
	}{
		{name: "iPhone-shaped fullbox mdta", placement: "top", metadata: cameraMP4MDTA(true)},
		{name: "QuickTime bare mdta", placement: "moov", metadata: cameraMP4MDTA(false)},
		{name: "movie udta mdta", placement: "moov", metadata: videoComposeTestBox("udta", cameraMP4MDTA(true))},
		{name: "Android top udta text", placement: "top", metadata: videoComposeTestBox("udta", videoComposeTestBox("\xa9too", []byte("Camera recorder")))},
		{name: "Android movie udta text", placement: "moov", metadata: videoComposeTestBox("udta", videoComposeTestBox("\xa9too", []byte("Camera recorder")))},
		{name: "track mdir metadata", placement: "trak", metadata: cameraMP4MDIR(videoComposeTestBox("\xa9nam", cameraMP4Data([]byte("Chapter one"))))},
		{name: "top XMP UUID", placement: "top", metadata: cameraMP4UUID(t, cameraMP4XMPUUID, []byte("<x:xmpmeta>literal data</x:xmpmeta>"), false)},
		{name: "top spherical UUID", placement: "top", metadata: cameraMP4UUID(t, cameraMP4SphericalUUID, []byte("<GSpherical:Spherical>true</GSpherical:Spherical>"), false)},
		{name: "extended XMP UUID", placement: "top", metadata: cameraMP4UUID(t, cameraMP4XMPUUID, []byte("literal extended data"), true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := cameraMP4Validate(t, cameraMP4Fixture("qt  ", test.placement, test.metadata)); err != nil {
				t.Fatal("reviewed synthetic camera profile refused", err)
			}
		})
	}
}

func TestVideoComposeCameraUUIDAllowlistAndFraming(t *testing.T) {
	for _, namespace := range []string{
		"00000000000000000000000000000000", // Unknown namespace.
		"a5d40b30e81411ddba2f0800200c9a66", // ISML manifest.
		"6d1d9b0542d544e680e2141daff757b2", // ISML tfxd.
		"d4807ef2ca3946958e5426cb9e46a79f", // ISML tfrf.
	} {
		t.Run(namespace, func(t *testing.T) {
			box := cameraMP4UUID(t, namespace, []byte("inert-looking but unapproved"), false)
			cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", box), "unsupported_uuid_box")
		})
	}
	for _, payload := range [][]byte{nil, make([]byte, 15)} {
		cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", videoComposeTestBox("uuid", payload)), "invalid_uuid_box")
	}
	for _, namespace := range []string{cameraMP4XMPUUID, cameraMP4SphericalUUID} {
		cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", cameraMP4UUID(t, namespace, nil, false)), "invalid_uuid_box")
	}
	largeUnknown := cameraMP4UUID(t, "00000000000000000000000000000000", make([]byte, 1<<20), false)
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", largeUnknown), "unsupported_uuid_box")
	// Valid UUID namespaces do not authorize interpreting nested boxes within
	// their opaque payload during preflight. Decoder acceptance is a separate gate.
	opaque := videoComposeTestBox("dref", []byte("http://example.invalid/not-media"))
	if err := cameraMP4Validate(t, cameraMP4Fixture("isom", "top", cameraMP4UUID(t, cameraMP4XMPUUID, opaque, false))); err != nil {
		t.Fatal("opaque approved UUID bytes were recursively interpreted", err)
	}
	truncated := []byte{0, 0, 0, 1, 'u', 'u', 'i', 'd', 0, 0, 0, 0, 0, 0, 0}
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", truncated), "truncated MP4 extended")
}

func TestVideoComposeCameraUUIDsStayTopLevel(t *testing.T) {
	uuid := cameraMP4UUID(t, cameraMP4XMPUUID, []byte("metadata"), false)
	for _, test := range []struct {
		name      string
		placement string
		metadata  []byte
	}{
		{name: "ordinary movie", placement: "moov", metadata: uuid},
		{name: "ordinary track", placement: "trak", metadata: uuid},
		{name: "udta child", placement: "top", metadata: videoComposeTestBox("udta", uuid)},
		{name: "meta handler", placement: "top", metadata: videoComposeTestBox("meta", make([]byte, 4), uuid)},
		{name: "ilst item", placement: "top", metadata: cameraMP4MDIR(uuid)},
		{name: "instead of data", placement: "top", metadata: videoComposeTestBox("meta", make([]byte, 4), cameraMP4Handler("mdta"), cameraMP4Keys("chapter"),
			videoComposeTestBox("ilst", cameraMP4NumericItem(1, uuid)))},
	} {
		t.Run(test.name, func(t *testing.T) {
			cameraMP4RequireReason(t, cameraMP4Fixture("isom", test.placement, test.metadata), "unsupported_uuid_box")
		})
	}
}

func TestVideoComposeCameraMDTAKeepsFileGlobalKeyScopeUnambiguous(t *testing.T) {
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "trak", cameraMP4MDTA(true)), "unsupported_metadata_box")
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "trak", videoComposeTestBox("udta", cameraMP4MDTA(true))), "unsupported_metadata_box")
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", cameraMP4MDTA(true), cameraMP4MDTA(true)), "unsupported_metadata_box")
	mdir := cameraMP4MDIR(videoComposeTestBox("\xa9nam", cameraMP4Data([]byte("Chapter"))))
	if err := cameraMP4Validate(t, cameraMP4Fixture("isom", "top", cameraMP4MDTA(true), mdir)); err != nil {
		t.Fatal("one mdta table plus independent mdir metadata refused", err)
	}
}

func TestVideoComposeCameraMetadataRejectsReferenceAndMovieRoles(t *testing.T) {
	for _, kind := range []string{"dref", "url ", "urn ", "dinf", "iloc", "moov", "trak", "mdat", "cmov", "rmra", "rmda", "rdrf", "free", "hoov"} {
		for _, role := range []string{"meta", "udta", "ilst item"} {
			t.Run(role+"/"+kind, func(t *testing.T) {
				// Eight payload bytes satisfy a data child's minimum size, so
				// the ilst fixture isolates the forbidden child kind.
				bad := videoComposeTestBox(kind, make([]byte, 8))
				metadata := cameraMP4MDTA(true, bad)
				if role == "udta" {
					metadata = videoComposeTestBox("udta", bad)
				}
				if role == "ilst item" {
					metadata = videoComposeTestBox("meta", make([]byte, 4), cameraMP4Handler("mdta"), cameraMP4Keys("chapter"),
						videoComposeTestBox("ilst", cameraMP4NumericItem(1, bad)))
				}
				cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", metadata), "unsupported_metadata_box")
			})
		}
	}
	unknown := videoComposeTestBox("zzzz", []byte("unknown role"))
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", cameraMP4MDTA(true, unknown)), "unsupported_metadata_box")
}

func TestVideoComposeCameraMetadataRequiresClosedFramedRoles(t *testing.T) {
	validItem := cameraMP4NumericItem(1, cameraMP4Data([]byte("Chapter")))
	mdta := func(keys []byte, items ...[]byte) []byte {
		return videoComposeTestBox("meta", make([]byte, 4), cameraMP4Handler("mdta"), keys, videoComposeTestBox("ilst", items...))
	}
	wrongFlags := cameraMP4Handler("mdta")
	wrongFlags[11] = 1 // Handler FullBox flags, not container framing.
	countMismatch := cameraMP4Keys("chapter")
	binary.BigEndian.PutUint32(countMismatch[12:16], 2)
	wrongNamespace := videoComposeTestBox("keys", []byte{0, 0, 0, 0, 0, 0, 0, 1}, videoComposeTestBox("url ", []byte("chapter")))
	zeroEntry := videoComposeTestBox("mdta", []byte("chapter"))
	binary.BigEndian.PutUint32(zeroEntry[:4], 0)
	zeroEntryKeys := videoComposeTestBox("keys", []byte{0, 0, 0, 0, 0, 0, 0, 1}, zeroEntry)
	extendedEntry := make([]byte, 16)
	binary.BigEndian.PutUint32(extendedEntry[:4], 1)
	copy(extendedEntry[4:8], "mdta")
	binary.BigEndian.PutUint64(extendedEntry[8:16], 23)
	extendedEntry = append(extendedEntry, []byte("chapter")...)
	extendedEntryKeys := videoComposeTestBox("keys", []byte{0, 0, 0, 0, 0, 0, 0, 1}, extendedEntry)
	for _, test := range []struct {
		name     string
		metadata []byte
		want     string
	}{
		{name: "nonzero meta FullBox flags", metadata: videoComposeTestBox("meta", []byte{0, 0, 0, 2}, cameraMP4Handler("mdta"), cameraMP4Keys("chapter"), videoComposeTestBox("ilst", validItem)), want: "invalid MP4 box size"},
		{name: "keys before handler", metadata: videoComposeTestBox("meta", make([]byte, 4), cameraMP4Keys("chapter"), cameraMP4Handler("mdta"), videoComposeTestBox("ilst", validItem))},
		{name: "handler missing", metadata: videoComposeTestBox("meta", make([]byte, 4), cameraMP4Keys("chapter"), videoComposeTestBox("ilst", validItem))},
		{name: "handler short", metadata: videoComposeTestBox("meta", make([]byte, 4), videoComposeTestBox("hdlr", make([]byte, 23)))},
		{name: "handler flags", metadata: videoComposeTestBox("meta", make([]byte, 4), wrongFlags, cameraMP4Keys("chapter"), videoComposeTestBox("ilst", validItem)), want: "unsupported_metadata_box"},
		{name: "handler type", metadata: videoComposeTestBox("meta", make([]byte, 4), cameraMP4Handler("vide"), cameraMP4Keys("chapter"), videoComposeTestBox("ilst", validItem)), want: "unsupported_metadata_box"},
		{name: "key count mismatch", metadata: mdta(countMismatch, validItem)},
		{name: "key namespace", metadata: mdta(wrongNamespace, validItem)},
		{name: "zero-sized key entry", metadata: mdta(zeroEntryKeys, validItem)},
		{name: "extended key entry", metadata: mdta(extendedEntryKeys, validItem)},
		{name: "keys empty", metadata: mdta(cameraMP4Keys(""), validItem)},
		{name: "keys NUL", metadata: mdta(cameraMP4Keys("chapter\x00escape"), validItem), want: "unsupported_metadata_box"},
		{name: "keys non UTF8", metadata: mdta(cameraMP4Keys("chapter\xff"), validItem), want: "unsupported_metadata_box"},
		{name: "artwork stream key", metadata: mdta(cameraMP4Keys("com.apple.quicktime.artwork"), validItem), want: "unsupported_metadata_box"},
		{name: "zero mdta item index", metadata: mdta(cameraMP4Keys("chapter"), cameraMP4NumericItem(0, cameraMP4Data([]byte("value")))), want: "unsupported_metadata_box"},
		{name: "out of range mdta item", metadata: mdta(cameraMP4Keys("chapter"), cameraMP4NumericItem(2, cameraMP4Data([]byte("value")))), want: "unsupported_metadata_box"},
		{name: "data missing", metadata: mdta(cameraMP4Keys("chapter"), cameraMP4NumericItem(1)), want: "invalid or excessive MP4 boxes"},
		{name: "data short", metadata: mdta(cameraMP4Keys("chapter"), cameraMP4NumericItem(1, videoComposeTestBox("data", make([]byte, 7))))},
		{name: "two data children", metadata: mdta(cameraMP4Keys("chapter"), cameraMP4NumericItem(1, cameraMP4Data([]byte("one")), cameraMP4Data([]byte("two"))))},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := test.want
			if want == "" {
				want = "invalid_metadata_box"
			}
			cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", test.metadata), want)
		})
	}
	// xyz is now an explicitly reviewed location-text leaf. Keep this
	// refusal oracle on an unapproved namespace; xyz has positive fixtures.
	unknownItem := cameraMP4MDIR(videoComposeTestBox("\xa9zzz", cameraMP4Data([]byte("not an approved text item"))))
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", unknownItem), "unsupported_metadata_box")
}

func TestVideoComposeCameraMetadataBudgetsIncludeHeadersWithoutDoubleCharge(t *testing.T) {
	const boxLimit = 1 << 20
	for _, extended := range []bool{false, true} {
		if err := cameraMP4Validate(t, cameraMP4Fixture("isom", "top", cameraMP4SizedUUID(t, boxLimit, extended))); err != nil {
			t.Fatal("exact one-MiB metadata box refused", err)
		}
		cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", cameraMP4SizedUUID(t, boxLimit+1, extended)), "metadata_box_too_large")
	}
	// Five physical roots sum to exactly four MiB. Increasing the last by one
	// byte isolates the total cap while every individual box remains in bounds.
	roots := [][]byte{cameraMP4SizedUUID(t, boxLimit, false), cameraMP4SizedUUID(t, boxLimit, false),
		cameraMP4SizedUUID(t, boxLimit, false), cameraMP4SizedUUID(t, boxLimit-25, false), cameraMP4SizedUUID(t, 25, false)}
	if err := cameraMP4Validate(t, cameraMP4Fixture("isom", "top", roots...)); err != nil {
		t.Fatal("exact four-MiB physical metadata budget refused", err)
	}
	roots[len(roots)-1] = cameraMP4SizedUUID(t, 26, false)
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", roots...), "metadata_budget_exceeded")
	// A six-container metadata region is still one MiB on disk,
	// not six MiB of metadata. This kills nested-size double charging.
	leaf := videoComposeTestBox("\xa9too", bytes.Repeat([]byte{'A'}, boxLimit-7*8))
	nested := cameraMP4NestedUDTA(6, leaf)
	if len(nested) != boxLimit {
		t.Fatal("nested metadata boundary fixture is not exactly one MiB")
	}
	if err := cameraMP4Validate(t, cameraMP4Fixture("isom", "top", nested)); err != nil {
		t.Fatal("nested metadata was charged more than once", err)
	}
}

func TestVideoComposeCameraMetadataSharesGlobalFramingAndDepthLimits(t *testing.T) {
	leaf := videoComposeTestBox("\xa9too", []byte("camera"))
	if err := cameraMP4Validate(t, cameraMP4Fixture("isom", "top", cameraMP4NestedUDTA(6, leaf))); err != nil {
		t.Fatal("depth-six metadata fixture refused", err)
	}
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", cameraMP4NestedUDTA(7, leaf)), "nesting")
	items := make([][]byte, 4100)
	for i := range items {
		items[i] = cameraMP4UUID(t, cameraMP4XMPUUID, []byte{0}, false)
	}
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", items...), "boxes")
	cameraMP4RequireReason(t, cameraMP4Fixture("isom", "top", videoComposeTestBox("hoov", []byte("movie alias"))), "unsupported")
	if err := cameraMP4Validate(t, cameraMP4Fixture("isom", "top", videoComposeTestBox("free", make([]byte, 8)))); err != nil {
		t.Fatal("ordinary top-level zero padding refused", err)
	}
}
