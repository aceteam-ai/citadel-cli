package desktopmodel

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func fixtureManifest() manifest { return manifest{1, "gzip-ustar", 1, strings.Repeat("0", 64)} }

func TestManifestGolden(t *testing.T) {
	m := fixtureManifest()
	want := `{"schema_version":1,"archive_format":"gzip-ustar","compressed_bytes":1,"archive_sha256":"0000000000000000000000000000000000000000000000000000000000000000"}`
	b, err := m.canonical()
	if err != nil || string(b) != want {
		t.Fatalf("canonical %s %v", b, err)
	}
	fp, err := m.fingerprint()
	if err != nil || fp != "a2d90e6fb5c67d9a0e82a2a793dab02ee328aa36cb1294174c4466e5d7e82348" {
		t.Fatalf("fingerprint %s %v", fp, err)
	}
	input := ` { "archive_sha256":"` + strings.Repeat("0", 64) + `", "compressed_bytes":1,"archive_format":"gzip-ustar","schema_version":1 } `
	parsed, err := parseManifest(strings.NewReader(input))
	if err != nil || parsed != m {
		t.Fatalf("permutation %v", err)
	}
	fp2, _ := parsed.fingerprint()
	if fp2 != fp {
		t.Fatal("unstable fingerprint")
	}
	for _, other := range []manifest{{1, "gzip-ustar", 2, m.ArchiveSHA256}, {1, "gzip-ustar", 1, strings.Repeat("a", 64)}} {
		got, err := other.fingerprint()
		if err != nil || got == fp {
			t.Fatal("field omitted from fingerprint")
		}
	}
	for _, other := range []manifest{{2, "gzip-ustar", 1, m.ArchiveSHA256}, {1, "other", 1, m.ArchiveSHA256}} {
		if _, err := other.fingerprint(); err != errManifest {
			t.Fatal("unsupported descriptor fingerprint")
		}
	}
}

func TestManifestStrict(t *testing.T) {
	b, _ := fixtureManifest().canonical()
	base := string(b)
	cases := map[string]string{
		"duplicate":         strings.Replace(base, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"escaped_duplicate": strings.Replace(base, `"schema_version":1`, `"schema_version":1,"schema\u005fversion":1`, 1),
		"unknown":           strings.Replace(base, `"schema_version":1`, `"schema_version":1,"token":"secret"`, 1),
		"missing":           strings.Replace(base, `"schema_version":1,`, "", 1),
		"null":              strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":null`, 1),
		"nested":            strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":{}`, 1),
		"wrong_type":        strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":"1"`, 1),
		"fraction":          strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":1.0`, 1),
		"exponent":          strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":1e0`, 1),
		"leading_zero":      strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":01`, 1),
		"negative":          strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":-1`, 1),
		"zero":              strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":0`, 1),
		"oversized":         strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":8388609`, 1),
		"overflow":          strings.Replace(base, `"compressed_bytes":1`, `"compressed_bytes":18446744073709551616`, 1),
		"uppercase":         strings.Replace(base, strings.Repeat("0", 64), strings.Repeat("A", 64), 1),
		"short_hash":        strings.Replace(base, strings.Repeat("0", 64), "a", 1),
		"array":             "[]", "empty": "", "trailing": base + `{}`, "garbage": base + "x",
		"bom": "\xef\xbb\xbf" + base, "utf8": base + "\xff",
		"null_string": strings.Replace(base, `"archive_format":"gzip-ustar"`, `"archive_format":null`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := parseManifest(strings.NewReader(input))
			if err != errManifest || m != (manifest{}) {
				t.Fatalf("accepted %q: %v", name, err)
			}
			if err.Error() != "manifest_invalid" {
				t.Fatal("raw input leaked")
			}
		})
	}
	for _, n := range []uint64{1, compressedLimit} {
		m := fixtureManifest()
		m.CompressedBytes = n
		b, _ := m.canonical()
		if got, err := parseManifest(bytes.NewReader(b)); err != nil || got != m {
			t.Fatal("boundary refused")
		}
	}
	input := base + strings.Repeat(" ", manifestLimit-len(base))
	if _, err := parseManifest(strings.NewReader(input)); err != nil {
		t.Fatal("exact parser cap")
	}
	if _, err := parseManifest(strings.NewReader(input + " ")); err != errManifest {
		t.Fatal("parser cap+1")
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, errors.New("private raw detail") }

type countedReader struct {
	r     io.Reader
	bytes int
}

func (r *countedReader) Read(p []byte) (int, error) { n, e := r.r.Read(p); r.bytes += n; return n, e }

func TestBoundedReads(t *testing.T) {
	for _, limit := range []int64{manifestLimit, stateLimit, compressedLimit} {
		r := &countedReader{r: io.LimitReader(zeroReader{}, limit)}
		b, err := readBounded(r, limit, errArchive)
		if err != nil || int64(len(b)) != limit || int64(r.bytes) != limit {
			t.Fatal("exact bounded read")
		}
		r = &countedReader{r: io.LimitReader(zeroReader{}, limit+20)}
		b, err = readBounded(r, limit, errArchive)
		if err != errArchive || b != nil || int64(r.bytes) != limit+1 {
			t.Fatal("read exceeds refusal bound")
		}
	}
	if b, err := readBounded(failedReader{}, 10, errManifest); err != errManifest || b != nil {
		t.Fatal("raw reader error leaked")
	}
	for _, limit := range []int64{-1, int64(^uint64(0) >> 1)} {
		if _, err := readBounded(bytes.NewReader(nil), limit, errState); err != errState {
			t.Fatal("unsafe limit")
		}
	}
	if _, err := parseManifest(nil); err != errManifest {
		t.Fatal("nil reader")
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }
