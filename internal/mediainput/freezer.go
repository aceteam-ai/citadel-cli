package mediainput

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// Metadata is safe to place in a receipt: no path, credential or image bytes.
type Metadata struct {
	Role   string
	Origin string
	SHA256 string
	Size   int64
	Format string
	Width  int
	Height int
}

type frozenInput struct {
	metadata Metadata
	data     []byte
}

// Snapshot holds only verified private byte buffers. It exposes independent
// read-only readers, never backing slices, source paths or bearer credentials.
// This is not execution authority; handler-level live publication gates remain
// mandatory. No task registration or capability advertisement occurs here.
type Snapshot struct {
	inputs []frozenInput
}

type snapshotReader struct{ reader *bytes.Reader }

func (r *snapshotReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *snapshotReader) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<private media-input reader>")
}

func (s Snapshot) Inputs() []Metadata {
	result := make([]Metadata, len(s.inputs))
	for i, input := range s.inputs {
		result[i] = input.metadata
	}
	return result
}

func (s Snapshot) Reader(role string) (io.Reader, error) {
	for _, input := range s.inputs {
		if input.metadata.Role == role {
			return &snapshotReader{reader: bytes.NewReader(input.data)}, nil
		}
	}
	return nil, refuse("missing_input_role")
}

// Format prevents common diagnostic verbs from reflecting private image bytes.
func (s Snapshot) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<private media-input snapshot>")
}

// WorkspaceFreezer freezes already parsed workspace inputs. Inbox acquisition
// is deliberately unsupported until the separately reviewed shared transport
// lands; mixed contracts are refused before any file is read.
type WorkspaceFreezer struct {
	WorkspaceDir string
	FilesEnabled func() bool
	// Private deterministic test seam, invoked before the second descriptor stat.
	afterRead func()
}

func (f WorkspaceFreezer) Freeze(ctx context.Context, contract Contract) (Snapshot, error) {
	if len(contract.inputs) < 1 || len(contract.inputs) > 2 {
		return Snapshot{}, refuse("invalid_schema")
	}
	for _, input := range contract.inputs {
		if input.Origin != "workspace" {
			return Snapshot{}, refuse("inbox_unavailable")
		}
	}
	if f.FilesEnabled == nil || !f.FilesEnabled() {
		return Snapshot{}, refuse("files_disabled")
	}
	if ctx.Err() != nil {
		return Snapshot{}, refuse("cancelled_or_timeout")
	}
	if f.WorkspaceDir == "" {
		return Snapshot{}, refuse("workspace_unavailable")
	}
	root, err := os.OpenRoot(f.WorkspaceDir)
	if err != nil {
		return Snapshot{}, refuse("workspace_unavailable")
	}
	defer root.Close()
	snapshot := Snapshot{inputs: make([]frozenInput, 0, len(contract.inputs))}
	for _, input := range contract.inputs {
		if !f.FilesEnabled() {
			return Snapshot{}, refuse("files_disabled")
		}
		frozen, err := f.read(ctx, root, input)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.inputs = append(snapshot.inputs, frozen)
	}
	if len(snapshot.inputs) == 2 {
		source, mask := snapshot.inputs[0].metadata, snapshot.inputs[1].metadata
		if source.Width != mask.Width || source.Height != mask.Height {
			return Snapshot{}, refuse("mask_invalid")
		}
	}
	if ctx.Err() != nil {
		return Snapshot{}, refuse("cancelled_or_timeout")
	}
	if !f.FilesEnabled() {
		return Snapshot{}, refuse("files_disabled")
	}
	return snapshot, nil
}

func (f WorkspaceFreezer) read(ctx context.Context, root *os.Root, input Descriptor) (frozenInput, error) {
	if ctx.Err() != nil {
		return frozenInput{}, refuse("cancelled_or_timeout")
	}
	file, err := openWorkspaceInput(root, input.Path)
	if err != nil {
		return frozenInput{}, refuse("invalid_input_path")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return frozenInput{}, refuse("invalid_input_path")
	}
	if before.Size() != input.Size || before.Size() < 1 || before.Size() > MaxFileBytes {
		return frozenInput{}, refuse("size_mismatch")
	}
	// The descriptor size was validated by Parse and fstat. A private buffer is
	// allocated once; no named staging file or reopen-by-path is ever used.
	data := make([]byte, int(input.Size))
	for offset := 0; offset < len(data); {
		if ctx.Err() != nil {
			return frozenInput{}, refuse("cancelled_or_timeout")
		}
		end := offset + 64<<10
		if end > len(data) {
			end = len(data)
		}
		n, err := io.ReadFull(file, data[offset:end])
		if err != nil {
			return frozenInput{}, refuse("input_changed")
		}
		offset += n
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || err != io.EOF {
		return frozenInput{}, refuse("input_changed")
	}
	if f.afterRead != nil {
		f.afterRead()
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return frozenInput{}, refuse("input_changed")
	}
	if ctx.Err() != nil {
		return frozenInput{}, refuse("cancelled_or_timeout")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != input.SHA256 {
		return frozenInput{}, refuse("sha256_mismatch")
	}
	width, height, err := inspectImage(data, input.Format, input.Role == "mask")
	if err != nil {
		return frozenInput{}, err
	}
	return frozenInput{metadata: Metadata{
		Role: input.Role, Origin: input.Origin, SHA256: input.SHA256,
		Size: input.Size, Format: input.Format, Width: width, Height: height,
	}, data: data}, nil
}
