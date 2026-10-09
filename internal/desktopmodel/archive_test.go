package desktopmodel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"
)

type tarMember struct {
	name string
	data []byte
	typ  byte
}

func makeTar(t *testing.T, members []tarMember) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, m := range members {
		typ := m.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: m.name, Mode: 0600, Size: int64(len(m.data)), Typeflag: typ, Format: tar.FormatUSTAR}
		if typ == tar.TypeDir {
			h.Mode = 0700
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(m.data) > 0 {
			if _, err := tw.Write(m.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func makeGzip(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	if _, err := z.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func manifestFor(b []byte) manifest {
	h := sha256.Sum256(b)
	return manifest{1, "gzip-ustar", uint64(len(b)), hex.EncodeToString(h[:])}
}

func archiveSuccess(t *testing.T, raw []byte) *archiveReceipt {
	t.Helper()
	b := makeGzip(t, raw)
	m := manifestFor(b)
	r, err := validateArchive(bytes.NewReader(b), m)
	if err != nil || r == nil {
		t.Fatalf("valid synthetic archive refused: %v", err)
	}
	if r.archiveSHA != m.ArchiveSHA256 {
		t.Fatal("receipt hash")
	}
	fp, _ := m.fingerprint()
	if r.fingerprint != fp {
		t.Fatal("receipt binding")
	}
	return r
}

func archiveFailure(t *testing.T, raw []byte) {
	t.Helper()
	b := makeGzip(t, raw)
	r, err := validateArchive(bytes.NewReader(b), manifestFor(b))
	if err != errArchive || r != nil || err.Error() != "archive_invalid" {
		t.Fatal("invalid archive produced receipt or raw detail")
	}
}

func checksum(h []byte) {
	copy(h[148:156], "        ")
	sum := 0
	for _, b := range h[:512] {
		sum += int(b)
	}
	copy(h[148:156], fmt.Sprintf("%06o\x00 ", sum))
}

func altered(t *testing.T, raw []byte, change func([]byte)) []byte {
	t.Helper()
	b := bytes.Clone(raw)
	change(b[:512])
	checksum(b[:512])
	return b
}

func TestArchivePositive(t *testing.T) {
	raw := makeTar(t, []tarMember{{"dir/", nil, tar.TypeDir}, {"dir/empty", nil, 0}, {"dir/file", []byte("benign fixture"), 0}})
	r := archiveSuccess(t, raw)
	if r.members != 3 || r.payload != 14 {
		t.Fatalf("receipt counts %#v", r)
	}
	// POSIX TypeRegA is normalized to TypeReg, never to a slash-inferred directory.
	raw = makeTar(t, []tarMember{{"fixture", []byte("x"), 0}})
	archiveSuccess(t, altered(t, raw, func(h []byte) { h[156] = 0 }))
	archiveSuccess(t, makeTar(t, []tarMember{{"dir", nil, tar.TypeDir}, {"dir/file", []byte("x"), 0}}))
}

func TestArchiveHeaderRefusals(t *testing.T) {
	raw := makeTar(t, []tarMember{{"fixture", []byte("benign"), 0}})
	cases := map[string]func([]byte){
		"magic": func(h []byte) { h[257] = 'x' }, "version": func(h []byte) { h[264] = '1' },
		"prefix": func(h []byte) { h[345] = 'x' }, "link_target": func(h []byte) { h[157] = 'x' },
		"base256": func(h []byte) { h[124] = 0x80 }, "size_space": func(h []byte) { h[124] = ' ' },
		"size_sign": func(h []byte) { h[124] = '-' }, "size_no_nul": func(h []byte) { h[135] = '0' },
		"size_huge":      func(h []byte) { copy(h[124:135], "77777777777") },
		"hidden_name":    func(h []byte) { h[20] = 'x' },
		"directory_size": func(h []byte) { h[156] = tar.TypeDir },
		"nul_slash":      func(h []byte) { clear(h[:100]); copy(h, "fixture/"); h[156] = 0 },
	}
	for _, typ := range []byte{tar.TypeLink, tar.TypeSymlink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo, tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink, tar.TypeGNUSparse, '9'} {
		typ := typ
		cases[fmt.Sprintf("type_%d", typ)] = func(h []byte) { h[156] = typ }
	}
	// A forbidden zero-size header followed by a benign nonempty file must
	// fail because of type, not accidentally because aggregate payload is0.
	mixed := makeTar(t, []tarMember{{"forbidden", nil, 0}, {"benign", []byte("x"), 0}})
	for _, typ := range []byte{tar.TypeLink, tar.TypeSymlink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		typ := typ
		t.Run(fmt.Sprintf("mixed_type_%d", typ), func(t *testing.T) { archiveFailure(t, altered(t, mixed, func(h []byte) { h[156] = typ })) })
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) { archiveFailure(t, altered(t, raw, change)) })
	}
	bad := bytes.Clone(raw)
	bad[148] = '9'
	archiveFailure(t, bad)
	bad = bytes.Clone(raw)
	bad[512+6] = 1
	archiveFailure(t, bad)
	for _, n := range []int{0, 511, 512, 517, len(raw) - 1, len(raw) - 512} {
		t.Run(fmt.Sprintf("truncated_%d", n), func(t *testing.T) { archiveFailure(t, raw[:n]) })
	}
	archiveFailure(t, append(bytes.Clone(raw), make([]byte, 512)...))
	bad = bytes.Clone(raw)
	bad[len(bad)-1] = 1
	archiveFailure(t, bad)
	archiveFailure(t, makeTar(t, nil))
	archiveFailure(t, makeTar(t, []tarMember{{"empty", nil, 0}}))
}

func TestArchivePathRefusals(t *testing.T) {
	for _, name := range []string{"/abs", "../escape", "a//b", "a\\b", "C:x", "a.", "a/..", "a/./b", "a\x01", ".hidden", strings.Repeat("a", 65)} {
		t.Run(fmt.Sprintf("name_%q", name), func(t *testing.T) { archiveFailure(t, makeTar(t, []tarMember{{name, []byte("x"), 0}})) })
	}
	raw := makeTar(t, []tarMember{{"fixture", []byte("x"), 0}})
	archiveFailure(t, altered(t, raw, func(h []byte) { clear(h[:100]); copy(h, "é") }))
	archiveFailure(t, altered(t, raw, func(h []byte) { clear(h[:100]); copy(h, "a/") }))
	for name, members := range map[string][]tarMember{
		"duplicate":        {{"a", []byte("x"), 0}, {"a", []byte("y"), 0}},
		"case":             {{"a", []byte("x"), 0}, {"A", []byte("y"), 0}},
		"parent_missing":   {{"a/b", []byte("x"), 0}},
		"parent_file":      {{"a", []byte("x"), 0}, {"a/b", []byte("y"), 0}},
		"parent_case":      {{"A/", nil, tar.TypeDir}, {"a/b", []byte("x"), 0}},
		"dir_conflict":     {{"a/", nil, tar.TypeDir}, {"a", []byte("x"), 0}},
		"double_dir_slash": {{"a//", nil, tar.TypeDir}, {"a/file", []byte("x"), 0}},
	} {
		t.Run(name, func(t *testing.T) { archiveFailure(t, makeTar(t, members)) })
	}
	for _, n := range []int{64, 65} {
		_, ok := canonicalName(strings.Repeat("a", n), false)
		if ok != (n == 64) {
			t.Fatal("component boundary")
		}
	}
	name := strings.Repeat("a", 64) + "/" + strings.Repeat("b", 35)
	if _, ok := canonicalName(name, false); !ok {
		t.Fatal("exact name100")
	}
	if _, ok := canonicalName(name+"b", false); ok {
		t.Fatal("name101")
	}
	archiveSuccess(t, makeTar(t, []tarMember{{strings.Repeat("a", 64) + "/", nil, tar.TypeDir}, {name, []byte("x"), 0}}))
	for _, depth := range []int{8, 9} {
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = "a"
		}
		_, ok := canonicalName(strings.Join(parts, "/"), false)
		if ok != (depth == 8) {
			t.Fatal("depth boundary")
		}
	}
	var members []tarMember
	for i := 1; i < 8; i++ {
		members = append(members, tarMember{strings.Repeat("a/", i), nil, tar.TypeDir})
	}
	members = append(members, tarMember{strings.Repeat("a/", 7) + "file", []byte("x"), 0})
	archiveSuccess(t, makeTar(t, members))
}

func TestArchiveNumericalLimits(t *testing.T) {
	for _, n := range []int{memberLimit, memberLimit + 1} {
		members := make([]tarMember, n)
		for i := range members {
			members[i] = tarMember{fmt.Sprintf("f%d", i), nil, 0}
		}
		members[0].data = []byte("x")
		raw := makeTar(t, members)
		if n == memberLimit {
			archiveSuccess(t, raw)
		} else {
			archiveFailure(t, raw)
		}
	}
	archiveSuccess(t, makeTar(t, []tarMember{{"exact", make([]byte, memberPayloadLimit), 0}}))
	archiveFailure(t, makeTar(t, []tarMember{{"excess", make([]byte, memberPayloadLimit+1), 0}}))
	members := make([]tarMember, 4)
	for i := range members {
		members[i] = tarMember{fmt.Sprintf("f%d", i), make([]byte, memberPayloadLimit), 0}
	}
	r := archiveSuccess(t, makeTar(t, members))
	if r.payload != payloadLimit {
		t.Fatal("aggregate exact")
	}
	members = append(members, tarMember{"excess", []byte("x"), 0})
	archiveFailure(t, makeTar(t, members))
	for _, n := range []uint64{0, 1, 511, 512, 513} {
		got, ok := paddedSize(n)
		if !ok || got%512 != 0 || got < n || got-n > 511 {
			t.Fatal("padding")
		}
	}
	if _, ok := paddedSize(^uint64(0)); ok {
		t.Fatal("padding overflow")
	}
	if _, ok := checkedAdd(^uint64(0), 1); ok {
		t.Fatal("addition overflow")
	}
	if _, _, err := validateTar(make([]byte, expandedLimit+1)); err != errArchive {
		t.Fatal("expanded representation cap")
	}
}

func TestArchiveExpandedGuard(t *testing.T) {
	// Direct decompressor seam: these are NOT claimed valid USTAR archives.
	for _, n := range []int{expandedLimit, expandedLimit + 1} {
		b := makeGzip(t, make([]byte, n))
		expanded, err := decompressArchive(b)
		if n == expandedLimit {
			if err != nil || len(expanded) != n {
				t.Fatal("exact expansion limit")
			}
		} else if err != errArchive || expanded != nil {
			t.Fatal("expansion limit+1 produced bytes")
		}
	}
}

func TestArchiveGzipIntegrity(t *testing.T) {
	good := makeGzip(t, makeTar(t, []tarMember{{"fixture", []byte("x"), 0}}))
	cases := map[string][]byte{
		"short": good[:9], "truncated": good[:len(good)-1], "trailing": append(bytes.Clone(good), 1),
		"concatenated": append(bytes.Clone(good), good...), "empty": nil,
	}
	for _, field := range []struct {
		name  string
		index int
	}{{"checksum", len(good) - 8}, {"isize", len(good) - 4}, {"method", 2}, {"magic", 0}} {
		b := bytes.Clone(good)
		b[field.index] ^= 1
		cases[field.name] = b
	}
	for _, flag := range []byte{1, 2, 4, 8, 16, 32, 64, 128} {
		b := bytes.Clone(good)
		b[3] = flag
		cases[fmt.Sprintf("flag_%d", flag)] = b
	}
	// Empty decoded FNAME would otherwise evade checking the decoded string.
	b := append(bytes.Clone(good[:10]), append([]byte{0}, good[10:]...)...)
	b[3] = 8
	cases["empty_name_flag"] = b
	b = append(bytes.Clone(good[:10]), append([]byte{0, 0}, good[10:]...)...)
	b[3] = 4
	cases["empty_extra_flag"] = b
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			m := manifestFor(b)
			r, err := validateArchive(bytes.NewReader(b), m)
			if err != errArchive || r != nil {
				t.Fatal("gzip failure produced receipt")
			}
		})
	}
	wrong := manifestFor(good)
	wrong.ArchiveSHA256 = strings.Repeat("0", 64)
	if r, e := validateArchive(bytes.NewReader(good), wrong); e != errArchive || r != nil {
		t.Fatal("wrong hash")
	}
	wrong = manifestFor(good)
	wrong.CompressedBytes++
	if r, e := validateArchive(bytes.NewReader(good), wrong); e != errArchive || r != nil {
		t.Fatal("wrong length")
	}
	if r, e := validateArchive(failedReader{}, manifestFor(good)); e != errArchive || r != nil {
		t.Fatal("reader error")
	}
	r := &countedReader{r: io.LimitReader(zeroReader{}, compressedLimit+50)}
	if receipt, e := validateArchive(r, fixtureManifest()); e != errArchive || receipt != nil || r.bytes != compressedLimit+1 {
		t.Fatal("compressed input bound")
	}
}
