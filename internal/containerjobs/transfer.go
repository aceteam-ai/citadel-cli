package containerjobs

import (
	"archive/tar"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

func manifestDigest(m *Manifest) string {
	b, _ := json.Marshal(m)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func chunkAAD(org, transfer string, index int) []byte {
	b, _ := json.Marshal([]any{"aceteam-container-transfer-v1", org, transfer, index})
	return b
}
func transferCipher(r Request) (cipher.AEAD, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(r.KeyB64)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalid
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalid
	}
	return cipher.NewGCM(block)
}

type chunkWriter struct {
	ctx       context.Context
	transport Transport
	request   Request
	gcm       cipher.AEAD
	buf       []byte
	manifest  Manifest
	total     int64
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if w.ctx.Err() != nil {
			return written, ErrUnavailable
		}
		n := min(len(p), ChunkLimit-(len(w.buf)-12))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		written += n
		if len(w.buf)-12 == ChunkLimit {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}
func (w *chunkWriter) flush() error {
	n := len(w.buf) - 12
	if n == 0 {
		return nil
	}
	i := len(w.manifest.Chunks)
	if i >= len(w.request.ChunkURLs) || w.ctx.Err() != nil {
		return ErrBounds
	}
	if _, err := rand.Read(w.buf[:12]); err != nil {
		return ErrUnavailable
	}
	sealed := w.gcm.Seal(w.buf[:12], w.buf[:12], w.buf[12:], chunkAAD(w.request.OrgID, w.request.TransferID, i))
	digest := sha256.Sum256(sealed)
	if err := w.transport.Put(w.ctx, w.request.ChunkURLs[i], sealed); err != nil || w.ctx.Err() != nil {
		return ErrUnavailable
	}
	w.manifest.Chunks = append(w.manifest.Chunks, Chunk{Index: i, PlaintextBytes: int64(n), SHA256: hex.EncodeToString(digest[:])})
	w.total += int64(n)
	w.buf = sealed[:12]
	return nil
}
func (e *Engine) exportVolume(ctx context.Context, r Request, out Result) (Result, error) {
	if e.Transport == nil {
		return out, ErrUnavailable
	}
	gcm, err := transferCipher(r)
	if err != nil {
		return out, err
	}
	root, err := e.Runtime.OpenVolume(ctx, r.VolumeRef)
	if err != nil {
		return out, ErrUnavailable
	}
	defer root.Close()
	writer := &chunkWriter{ctx: ctx, transport: e.Transport, request: r, gcm: gcm, buf: make([]byte, 12, 64<<10), manifest: Manifest{Version: 1, OrgID: r.OrgID, TransferID: r.TransferID}}
	tw := tar.NewWriter(writer)
	entries, names := 0, 0
	inodes := map[string]string{}
	err = walkArchive(ctx, root, ".", 0, func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || ctx.Err() != nil {
			return ErrUnavailable
		}
		if name == "." {
			return nil
		}
		entries++
		names += len(name)
		if entries > 100000 || names > 8<<20 || !relative(name, false) {
			return ErrBounds
		}
		st, err := root.Lstat(name)
		if err != nil {
			return ErrUnavailable
		}
		link := ""
		if st.Mode()&os.ModeSymlink != 0 {
			link, err = root.Readlink(name)
			if err != nil || !archiveLink(name, link) {
				return ErrInvalid
			}
			if _, err = root.Stat(name); err != nil && !os.IsNotExist(err) {
				return ErrInvalid
			}
			names += len(link)
			if names > 8<<20 {
				return ErrBounds
			}
		} else if !st.Mode().IsRegular() && !st.IsDir() {
			return ErrInvalid
		}
		h, err := tar.FileInfoHeader(st, link)
		if err != nil {
			return ErrInvalid
		}
		h.Name = name
		h.Uid = 0
		h.Gid = 0
		h.Uname = ""
		h.Gname = ""
		h.Mode &= 0777
		if st.Mode().IsRegular() {
			if inode := archiveInode(st); inode != "" {
				if first, ok := inodes[inode]; ok {
					h.Typeflag = tar.TypeLink
					h.Linkname = first
					h.Size = 0
					names += len(first)
					if names > 8<<20 {
						return ErrBounds
					}
				} else {
					inodes[inode] = name
				}
			}
		}
		if err = tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return nil
		}
		f, err := openRead(root, name)
		if err != nil {
			return ErrUnavailable
		}
		defer f.Close()
		current, err := f.Stat()
		if err != nil || !current.Mode().IsRegular() || current.Size() != st.Size() {
			return ErrUnavailable
		}
		_, err = io.CopyN(tw, &contextReader{ctx: ctx, r: f}, st.Size())
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = writer.flush()
	}
	if err != nil {
		return out, safeError(err)
	}
	// A producer may provide spare capability slots; only used slots are returned.
	state, err := e.Runtime.Container(ctx, r.ContainerRef)
	if err != nil || state.Running || state.OrgID != r.OrgID || state.VolumeRef != r.VolumeRef {
		return out, ErrUnavailable
	}
	out.Manifest = &writer.manifest
	out.ManifestSHA256 = manifestDigest(out.Manifest)
	out.Bytes = writer.total
	return out, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, ErrUnavailable
	}
	return r.r.Read(p)
}

type chunkReader struct {
	ctx       context.Context
	transport Transport
	request   Request
	gcm       cipher.AEAD
	index     int
	current   []byte
	total     int64
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, ErrUnavailable
	}
	for len(r.current) == 0 {
		if r.index == len(r.request.Manifest.Chunks) {
			return 0, io.EOF
		}
		c := r.request.Manifest.Chunks[r.index]
		b, err := r.transport.Get(r.ctx, r.request.ChunkURLs[r.index], c.PlaintextBytes+28)
		if err != nil || int64(len(b)) != c.PlaintextBytes+28 {
			return 0, ErrInvalid
		}
		digest := sha256.Sum256(b)
		if hex.EncodeToString(digest[:]) != c.SHA256 {
			return 0, ErrInvalid
		}
		plain, err := r.gcm.Open(b[12:12], b[:12], b[12:], chunkAAD(r.request.OrgID, r.request.TransferID, r.index))
		if err != nil || int64(len(plain)) != c.PlaintextBytes {
			return 0, ErrInvalid
		}
		r.current = plain
		r.index++
		r.total += int64(len(plain))
	}
	n := copy(p, r.current)
	r.current = r.current[n:]
	return n, nil
}
func (e *Engine) importVolume(ctx context.Context, r Request) (Result, error) {
	out := Result{VolumeRef: r.VolumeRef, QuotaBytes: r.QuotaBytes}
	if e.Transport == nil || r.Manifest.Validate(r.OrgID, r.TransferID, len(r.ChunkURLs)) != nil || manifestDigest(r.Manifest) != r.ManifestSHA256 {
		return out, ErrInvalid
	}
	total := int64(0)
	for _, c := range r.Manifest.Chunks {
		if total > r.QuotaBytes+(16<<20)-c.PlaintextBytes {
			return out, ErrBounds
		}
		total += c.PlaintextBytes
	}
	gcm, err := transferCipher(r)
	if err != nil {
		return out, err
	}
	if err = e.Runtime.CreateVolume(ctx, r); err != nil {
		return out, ErrUnavailable
	}
	// Partial imports are left owned and unreachable by a container, never
	// reported successful or automatically purged. Retry needs a fresh volume ref.
	v, err := e.Runtime.Volume(ctx, r.VolumeRef)
	if err != nil || v.OrgID != r.OrgID || v.QuotaBytes != r.QuotaBytes {
		return out, ErrOwnership
	}
	root, err := e.Runtime.OpenVolume(ctx, r.VolumeRef)
	if err != nil {
		return out, ErrUnavailable
	}
	defer root.Close()
	reader := &chunkReader{ctx: ctx, transport: e.Transport, request: r, gcm: gcm}
	tr := tar.NewReader(reader)
	seen := map[string]bool{}
	names := 0
	written := int64(0)
	var symlinks [][2]string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, safeError(err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		names += len(name)
		if !relative(name, false) || len(seen) >= 100000 || names > 8<<20 || seen[name] || ctx.Err() != nil {
			return out, ErrInvalid
		}
		seen[name] = true
		switch h.Typeflag {
		case tar.TypeDir:
			if err = root.MkdirAll(name, 0700); err != nil {
				return out, ErrUnavailable
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || written > r.QuotaBytes-h.Size {
				return out, ErrBounds
			}
			written += h.Size
			parent := strings.LastIndex(name, "/")
			if parent >= 0 {
				if root.MkdirAll(name[:parent], 0700) != nil {
					return out, ErrUnavailable
				}
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
			if err != nil {
				return out, ErrUnavailable
			}
			_, copyErr := io.CopyN(f, &contextReader{ctx: ctx, r: tr}, h.Size)
			syncErr := f.Sync()
			closeErr := f.Close()
			if copyErr != nil || syncErr != nil || closeErr != nil {
				return out, ErrUnavailable
			}
		case tar.TypeSymlink:
			names += len(h.Linkname)
			if names > 8<<20 || h.Size != 0 || !archiveLink(name, h.Linkname) {
				return out, ErrInvalid
			}
			symlinks = append(symlinks, [2]string{name, h.Linkname})
		case tar.TypeLink:
			names += len(h.Linkname)
			if names > 8<<20 || h.Size != 0 || !relative(h.Linkname, false) {
				return out, ErrInvalid
			}
			target, err := root.Lstat(h.Linkname)
			if err != nil || !target.Mode().IsRegular() {
				return out, ErrInvalid
			}
			if root.MkdirAll(path.Dir(name), 0700) != nil || root.Link(h.Linkname, name) != nil {
				return out, ErrUnavailable
			}
		default:
			return out, ErrInvalid
		}
	}
	// Install symlinks after file extraction, so they cannot redirect writes.
	for _, link := range symlinks {
		if ctx.Err() != nil || root.MkdirAll(path.Dir(link[0]), 0700) != nil || root.Symlink(link[1], link[0]) != nil {
			return out, ErrUnavailable
		}
	}
	for _, link := range symlinks {
		if _, err := root.Stat(link[0]); err != nil && !os.IsNotExist(err) {
			return out, ErrInvalid
		}
	}
	// Authenticate all remaining chunks, including chunks after tar EOF. Only
	// zero tar padding is allowed; trailing hidden archives/data are refused.
	buf := make([]byte, 32<<10)
	for {
		n, err := reader.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return out, ErrInvalid
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, safeError(err)
		}
	}
	if reader.total != total || ctx.Err() != nil {
		return out, ErrInvalid
	}
	out.Bytes = total
	out.Manifest = r.Manifest
	out.ManifestSHA256 = r.ManifestSHA256
	return out, nil
}

// Read directories in bounded batches. fs.WalkDir eagerly reads a complete
// directory before invoking its callback, defeating the archive entry budget.
func walkArchive(ctx context.Context, root *os.Root, name string, depth int, visit func(string, fs.DirEntry, error) error) error {
	if depth > 128 || ctx.Err() != nil {
		return ErrBounds
	}
	f, err := openRead(root, name)
	if err != nil {
		return ErrUnavailable
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(256)
		if err != nil && err != io.EOF {
			return ErrUnavailable
		}
		for _, entry := range entries {
			child := path.Join(name, entry.Name())
			if e := visit(child, entry, nil); e != nil {
				return e
			}
			if entry.IsDir() {
				if e := walkArchive(ctx, root, child, depth+1, visit); e != nil {
					return e
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func archiveLink(name, target string) bool {
	if target == "" || !validText(target, 4096) || strings.HasPrefix(target, "/") || strings.Contains(target, `\`) {
		return false
	}
	return relative(path.Join(path.Dir(name), target), true)
}
