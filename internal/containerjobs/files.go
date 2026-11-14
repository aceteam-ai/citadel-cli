package containerjobs

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"sort"
)

func files(ctx context.Context, root *os.Root, kind string, r Request, out Result) (Result, error) {
	if ctx.Err() != nil || !relative(r.Path, kind == FileList) {
		return out, ErrInvalid
	}
	switch kind {
	case FileRead:
		f, err := openRead(root, r.Path)
		if err != nil {
			return out, ErrUnavailable
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Size() > FileLimit {
			return out, ErrBounds
		}
		b, err := readVolumeBytes(f)
		if err != nil {
			return out, err
		}
		out.ContentB64 = base64.StdEncoding.EncodeToString(b)
		out.Bytes = int64(len(b))
	case FileWrite:
		b, err := base64.StdEncoding.Strict().DecodeString(r.ContentB64)
		if err != nil || len(b) > FileLimit {
			return out, ErrBounds
		}
		// Root.OpenFile follows only symlinks whose targets stay within this root.
		// O_NONBLOCK in the native helper prevents an existing FIFO from wedging.
		f, err := openWrite(root, r.Path)
		if err != nil {
			return out, ErrUnavailable
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			return out, ErrInvalid
		}
		if err = f.Truncate(0); err != nil {
			return out, ErrUnavailable
		}
		n, err := f.Write(b)
		if err != nil || n != len(b) {
			return out, ErrUnavailable
		}
		if f.Sync() != nil {
			return out, ErrUnavailable
		}
		out.Bytes = int64(n)
	case FileList:
		f, err := openRead(root, r.Path)
		if err != nil {
			return out, ErrUnavailable
		}
		defer f.Close()
		entries, err := f.ReadDir(10001)
		if err != nil && err != io.EOF || len(entries) > 10000 {
			return out, ErrBounds
		}
		for _, v := range entries {
			if ctx.Err() != nil {
				return out, ErrUnavailable
			}
			if v.Type()&os.ModeSymlink != 0 {
				continue
			}
			st, err := v.Info()
			if err != nil {
				return out, ErrUnavailable
			}
			if st.IsDir() || st.Mode().IsRegular() {
				out.Entries = append(out.Entries, Entry{Name: v.Name(), Directory: st.IsDir(), Bytes: st.Size()})
			}
		}
		sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	}
	if ctx.Err() != nil {
		return out, ErrUnavailable
	}
	return out, nil
}

// Metadata can become stale while a file grows. Bound the actual read too.
func readVolumeBytes(reader io.Reader) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, FileLimit+1))
	if err != nil || len(contents) > FileLimit {
		return nil, ErrBounds
	}
	return contents, nil
}
