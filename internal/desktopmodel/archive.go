package desktopmodel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
)

const (
	expandedLimit      = 32 * 1024 * 1024
	payloadLimit       = 16 * 1024 * 1024
	memberPayloadLimit = 4 * 1024 * 1024
	memberLimit        = 256
)

type archiveReceipt struct {
	fingerprint string
	archiveSHA  string
	members     uint64
	payload     uint64
}

func checkedAdd(a, b uint64) (uint64, bool) {
	if b > ^uint64(0)-a {
		return 0, false
	}
	return a + b, true
}

func paddedSize(n uint64) (uint64, bool) {
	sum, ok := checkedAdd(n, 511)
	if !ok {
		return 0, false
	}
	return sum &^ uint64(511), true
}

func decompressArchive(b []byte) ([]byte, error) {
	if len(b) < 10 || len(b) > compressedLimit || b[3] != 0 {
		return nil, errArchive
	}
	src := bytes.NewReader(b)
	z, err := gzip.NewReader(src)
	if err != nil {
		return nil, errArchive
	}
	z.Multistream(false)
	expanded, readErr := readBounded(z, expandedLimit, errArchive)
	closeErr := z.Close()
	if readErr != nil || closeErr != nil || src.Len() != 0 {
		return nil, errArchive
	}
	return expanded, nil
}

func zero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func canonicalName(name string, directory bool) (string, bool) {
	if len(name) == 0 || len(name) > 100 {
		return "", false
	}
	if directory {
		name = strings.TrimSuffix(name, "/")
	} else if strings.HasSuffix(name, "/") {
		return "", false
	}
	parts := strings.Split(name, "/")
	if len(parts) > 8 {
		return "", false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 64 || strings.HasSuffix(p, ".") {
			return "", false
		}
		for i, c := range []byte(p) {
			alphaNum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
			if !alphaNum && (i == 0 || (c != '.' && c != '_' && c != '-')) {
				return "", false
			}
		}
	}
	return name, true
}

func rawHeader(h []byte) (string, uint64, byte, bool) {
	if len(h) != 512 || !bytes.Equal(h[257:263], []byte("ustar\x00")) || !bytes.Equal(h[263:265], []byte("00")) || !zero(h[345:500]) || !zero(h[157:257]) {
		return "", 0, 0, false
	}
	typ := h[156]
	if typ != 0 && typ != tar.TypeReg && typ != tar.TypeDir {
		return "", 0, 0, false
	}
	names := h[:100]
	end := bytes.IndexByte(names, 0)
	if end >= 0 {
		if !zero(names[end:]) {
			return "", 0, 0, false
		}
		names = names[:end]
	}
	name := string(names)
	if _, ok := canonicalName(name, typ == tar.TypeDir); !ok {
		return "", 0, 0, false
	}
	var size uint64
	if h[135] != 0 {
		return "", 0, 0, false
	}
	for _, c := range h[124:135] {
		if c < '0' || c > '7' {
			return "", 0, 0, false
		}
		size = size*8 + uint64(c-'0') // Exactly11 octal digits fit in uint64.
	}
	if typ == 0 {
		typ = tar.TypeReg
	}
	return name, size, typ, true
}

func validateTar(b []byte) (uint64, uint64, error) {
	if len(b) > expandedLimit {
		return 0, 0, errArchive
	}
	var offset, members, payload uint64
	seen := make(map[string]byte)
	exactDirectories := make(map[string]bool)
	for {
		if uint64(len(b))-offset < 512 {
			return 0, 0, errArchive
		}
		h := b[offset : offset+512]
		if zero(h) {
			if uint64(len(b))-offset != 1024 || !zero(b[offset:]) || members == 0 || payload == 0 {
				return 0, 0, errArchive
			}
			return members, payload, nil
		}
		name, size, typ, ok := rawHeader(h)
		if !ok || members >= memberLimit || size > memberPayloadLimit || (typ == tar.TypeDir && size != 0) {
			return 0, 0, errArchive
		}
		canonical, _ := canonicalName(name, typ == tar.TypeDir)
		key := strings.ToLower(canonical)
		if _, exists := seen[key]; exists {
			return 0, 0, errArchive
		}
		parts := strings.Split(canonical, "/")
		for i := 1; i < len(parts); i++ {
			if !exactDirectories[strings.Join(parts[:i], "/")] {
				return 0, 0, errArchive
			}
		}
		pad, ok := paddedSize(size)
		if !ok {
			return 0, 0, errArchive
		}
		start, ok := checkedAdd(offset, 512)
		if !ok {
			return 0, 0, errArchive
		}
		end, ok := checkedAdd(start, pad)
		if !ok || end > uint64(len(b)) {
			return 0, 0, errArchive
		}
		if !zero(b[start+size : end]) {
			return 0, 0, errArchive
		}
		tr := tar.NewReader(bytes.NewReader(b[offset:end]))
		parsed, err := tr.Next()
		if err != nil || parsed.Format != tar.FormatUSTAR || parsed.Name != name || parsed.Size < 0 || uint64(parsed.Size) != size || parsed.Typeflag != typ {
			return 0, 0, errArchive
		}
		if typ == tar.TypeReg {
			payload, ok = checkedAdd(payload, size)
			if !ok || payload > payloadLimit {
				return 0, 0, errArchive
			}
		}
		seen[key] = typ
		if typ == tar.TypeDir {
			exactDirectories[canonical] = true
		}
		members++
		offset = end
	}
}

func validateArchive(r io.Reader, m manifest) (*archiveReceipt, error) {
	fingerprint, err := m.fingerprint()
	if err != nil {
		return nil, errArchive
	}
	b, err := readBounded(r, compressedLimit, errArchive)
	if err != nil || uint64(len(b)) != m.CompressedBytes {
		return nil, errArchive
	}
	h := sha256.Sum256(b)
	actual := hex.EncodeToString(h[:])
	if actual != m.ArchiveSHA256 {
		return nil, errArchive
	}
	expanded, err := decompressArchive(b)
	if err != nil {
		return nil, errArchive
	}
	members, payload, err := validateTar(expanded)
	if err != nil {
		return nil, errArchive
	}
	// Sole production success-receipt constructor; this is not a native proof.
	return &archiveReceipt{fingerprint, actual, members, payload}, nil
}
